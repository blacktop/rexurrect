package launchagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/blacktop/rexurrect/internal/autolog"
	"github.com/blacktop/rexurrect/internal/rexbin"
	"github.com/blacktop/rexurrect/internal/state"
)

type Agent struct {
	Label, Domain, PlistPath string
	launchctl                func(context.Context, ...string) ([]byte, error)
}

type Status struct {
	Label     string `json:"label"`
	Domain    string `json:"domain"`
	PlistPath string `json:"plist_path"`
	Installed bool   `json:"installed"`
	Loaded    bool   `json:"loaded"`
	Changed   bool   `json:"changed"`
}

func New(label, plistDir string) (*Agent, error) {
	if !validLabel(label) {
		return nil, errors.New("invalid LaunchAgent label")
	}
	if plistDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		plistDir = filepath.Join(home, "Library", "LaunchAgents")
	}
	if !filepath.IsAbs(plistDir) {
		return nil, errors.New("--plist-dir must be absolute")
	}
	return &Agent{Label: label, Domain: "gui/" + strconv.Itoa(os.Getuid()), PlistPath: filepath.Join(plistDir, label+".plist"), launchctl: runLaunchctl}, nil
}

func (a *Agent) target() string { return a.Domain + "/" + a.Label }

type limitedOutput struct{ buf bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > (1<<20)-b.buf.Len() {
		return 0, errors.New("launchctl output exceeds 1 MiB")
	}
	return b.buf.Write(p)
}

func runLaunchctl(ctx context.Context, args ...string) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("LaunchAgents require macOS")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/launchctl", args...)
	cmd.WaitDelay = time.Second
	var output limitedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if err != nil {
		return output.buf.Bytes(), fmt.Errorf("launchctl %s: %w: %s", args[0], errors.Join(ctx.Err(), err), output.buf.String())
	}
	return output.buf.Bytes(), nil
}

func (a *Agent) loaded(ctx context.Context) (bool, error) {
	_, err := a.launchctl(ctx, "print", a.target())
	if err == nil {
		return true, nil
	}
	// launchctl returns 113 for an absent service. Domain, permission,
	// timeout, and other errors must not masquerade as an unregistered job.
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 113 {
		return false, nil
	}
	return false, err
}

func (a *Agent) readOwned() ([]byte, error) {
	f, err := os.OpenFile(a.PlistPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > maxPlistSize {
		return nil, errors.New("LaunchAgent plist must be a bounded regular file without group or world writes")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPlistSize+1))
	if err != nil || len(data) > maxPlistSize {
		return nil, errors.Join(err, errors.New("cannot read bounded LaunchAgent plist"))
	}
	values, err := parsePlist(data)
	if err != nil {
		return nil, fmt.Errorf("read LaunchAgent plist: %w", err)
	}
	if values["Label"].XMLName.Local != "string" || values["Label"].Text != a.Label || values["RexResurrectManaged"].Text != "1" {
		return nil, errors.New("refusing to change a plist not owned by rexurrect with this label")
	}
	return data, nil
}

func (a *Agent) Status(ctx context.Context) (Status, error) {
	s := Status{Label: a.Label, Domain: a.Domain, PlistPath: a.PlistPath}
	_, err := a.readOwned()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	s.Installed = err == nil
	s.Loaded, err = a.loaded(ctx)
	return s, err
}

func (a *Agent) requireOwned(ctx context.Context) (Status, error) {
	s, err := a.Status(ctx)
	if err == nil && !s.Installed {
		err = errors.New("this LaunchAgent has no owned plist; install it first")
	}
	return s, err
}

func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("%s is not an executable file", path)
	}
	return nil
}

func (a *Agent) Install(ctx context.Context, c Config) (Status, error) {
	if c.Label != a.Label {
		return Status{}, errors.New("LaunchAgent labels do not match")
	}
	data, err := Render(c)
	if err != nil {
		return Status{}, err
	}
	if err := executable(c.Executable); err != nil {
		return Status{}, err
	}
	resolved := c.ResolvedRex
	if resolved == "" {
		resolved, _, err = rexbin.Resolve(c.Rex)
		if err != nil {
			return Status{}, err
		}
	}
	if err := executable(resolved); err != nil {
		return Status{}, err
	}
	old, err := a.readOwned()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	loaded, err := a.loaded(ctx)
	if err != nil {
		return Status{}, err
	}
	if loaded && old == nil {
		return Status{}, errors.New("refusing to replace a registered job without an owned plist")
	}
	changed := !bytes.Equal(old, data)
	if changed {
		store, err := state.Open(c.StateDir)
		if err != nil {
			return Status{}, err
		}
		if err := errors.Join(autolog.Prepare(c.StateDir), store.Close()); err != nil {
			return Status{}, err
		}
		if loaded {
			if _, err := a.launchctl(ctx, "bootout", a.target()); err != nil {
				return Status{}, err
			}
			loaded = false
		}
		if err := a.writePlist(data); err != nil {
			return Status{}, err
		}
	}
	if _, err := a.launchctl(ctx, "enable", a.target()); err != nil {
		return Status{}, err
	}
	if !loaded {
		if _, err := a.launchctl(ctx, "bootstrap", a.Domain, a.PlistPath); err != nil {
			return Status{}, err
		}
	}
	s, err := a.Status(ctx)
	s.Changed = changed || !loaded
	return s, err
}

func (a *Agent) writePlist(data []byte) error {
	dir := filepath.Dir(a.PlistPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+a.Label+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), a.PlistPath); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func (a *Agent) Enable(ctx context.Context) (Status, error) {
	s, err := a.requireOwned(ctx)
	if err != nil {
		return s, err
	}
	if _, err := a.launchctl(ctx, "enable", a.target()); err != nil {
		return s, err
	}
	if !s.Loaded {
		if _, err := a.launchctl(ctx, "bootstrap", a.Domain, a.PlistPath); err != nil {
			return s, err
		}
	}
	after, err := a.Status(ctx)
	after.Changed = !s.Loaded
	return after, err
}

func (a *Agent) Disable(ctx context.Context) (Status, error) {
	s, err := a.requireOwned(ctx)
	if err != nil {
		return s, err
	}
	if _, err := a.launchctl(ctx, "disable", a.target()); err != nil {
		return s, err
	}
	if s.Loaded {
		if _, err := a.launchctl(ctx, "bootout", a.target()); err != nil {
			return s, err
		}
	}
	after, err := a.Status(ctx)
	after.Changed = s.Loaded
	return after, err
}

func (a *Agent) Uninstall(ctx context.Context) (Status, error) {
	s, err := a.Status(ctx)
	if err != nil || !s.Installed && !s.Loaded {
		return s, err
	}
	if !s.Installed {
		return s, errors.New("refusing to remove a registered job without an owned plist")
	}
	// Enable clears any persistent disabled override without bootstrapping an
	// unloaded job. Uninstall must not leave the label marked disabled forever.
	if _, err := a.launchctl(ctx, "enable", a.target()); err != nil {
		return s, err
	}
	if s.Loaded {
		if _, err := a.launchctl(ctx, "bootout", a.target()); err != nil {
			return s, err
		}
	}
	if err := os.Remove(a.PlistPath); err != nil {
		return s, err
	}
	after, err := a.Status(ctx)
	after.Changed = true
	return after, err
}
