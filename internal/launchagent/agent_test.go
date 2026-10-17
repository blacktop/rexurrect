package launchagent

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/autolog"
	"github.com/blacktop/rexurrect/internal/state"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{Executable: bin, Server: "unix:///private/tmp/rex-<&>.sock", StateDir: filepath.Join(t.TempDir(), "state & space"), Rex: bin, Label: "io.blacktop.rexurrect.test.unit", Interval: DefaultInterval, Wait: 5 * time.Second, Timeout: time.Minute, CallTimeout: 45 * time.Second, LogMaxBytes: autolog.DefaultMaxBytes}
}

func TestLaunchAgentPlistBooleans(t *testing.T) {
	for _, kind := range []string{"true", "false"} {
		var out bytes.Buffer
		enc := xml.NewEncoder(&out)
		if err := encodePlistValue(enc, &out, plistValue{XMLName: xml.Name{Local: kind}}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatal(err)
		}
		if want := "<" + kind + "/>"; out.String() != want {
			t.Errorf("boolean %s: got %q, want %q", kind, out.String(), want)
		}
	}
}

func TestLaunchAgentPlist(t *testing.T) {
	c := testConfig(t)
	data, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("<key>RunAtLoad</key><true/>")) || bytes.Contains(data, []byte("<true></true>")) {
		t.Fatalf("RunAtLoad must use a self-closing plist boolean: %s", data)
	}
	if runtime.GOOS == "darwin" {
		cmd := exec.CommandContext(t.Context(), "/usr/bin/plutil", "-lint", "-")
		cmd.Stdin = bytes.NewReader(data)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("macOS rejected rendered plist: %v\n%s\n%s", err, out, data)
		}
	}
	p, err := parsePlist(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{c.Executable, "auto", "--server", c.Server, "--state-dir", c.StateDir, "--rex", c.Rex, "--wait", "5s", "--timeout", "1m0s", "--call-timeout", "45s", "--log-max-bytes", "1048576"}
	if !slices.Equal(p["ProgramArguments"].Strings, want) || p["ProgramArguments"].XMLName.Local != "array" {
		t.Fatalf("arguments: %#v", p["ProgramArguments"])
	}
	for key, want := range map[string]string{"Label": c.Label, "StartInterval": "60", "ProcessType": "Background", "StandardOutPath": filepath.Join(c.StateDir, autolog.OutName), "StandardErrorPath": filepath.Join(c.StateDir, autolog.ErrName)} {
		if p[key].Text != want {
			t.Errorf("%s = %q, want %q", key, p[key].Text, want)
		}
	}
	if p["RunAtLoad"].XMLName.Local != "true" || p["StartInterval"].XMLName.Local != "integer" {
		t.Fatal("missing typed interval or RunAtLoad")
	}
	if p["WatchPaths"].XMLName.Local != "array" || !slices.Equal(p["WatchPaths"].Strings, []string{strings.TrimPrefix(c.Server, "unix://")}) {
		t.Fatalf("WatchPaths: %#v", p["WatchPaths"])
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Executable = "relative" },
		func(c *Config) { c.StateDir = "relative" },
		func(c *Config) { c.Rex = "relative" },
		func(c *Config) { c.Label = "../other" },
		func(c *Config) { c.Server = "unix://relative" },
		func(c *Config) { c.Interval = 59 * time.Second },
		func(c *Config) { c.Interval = 25 * time.Hour },
		func(c *Config) { c.Interval += time.Nanosecond },
		func(c *Config) { c.Wait = 0 },
		func(c *Config) { c.Timeout = 2 * time.Hour },
		func(c *Config) { c.Wait = c.Timeout + time.Second },
		func(c *Config) { c.CallTimeout = 0 },
		func(c *Config) { c.CallTimeout = -time.Second },
		func(c *Config) { c.CallTimeout = time.Minute + time.Nanosecond },
		func(c *Config) { c.LogMaxBytes = 0 },
	} {
		bad := c
		mutate(&bad)
		if _, err := Render(bad); err == nil {
			t.Errorf("accepted invalid config: %+v", bad)
		}
	}
}

func TestLaunchAgentOperations(t *testing.T) {
	c := testConfig(t)
	a, err := New(c.Label, filepath.Join(t.TempDir(), "LaunchAgents"))
	if err != nil {
		t.Fatal(err)
	}
	missing := exec.Command("/bin/sh", "-c", "exit 113").Run()
	loaded, disabled := false, true
	var calls []string
	a.launchctl = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "bootstrap" {
			if !slices.Equal(args, []string{"bootstrap", a.Domain, a.PlistPath}) {
				t.Fatalf("wrong bootstrap target: %q", args)
			}
			loaded = true
			return nil, nil
		}
		if len(args) != 2 || args[1] != a.target() {
			t.Fatalf("wrong service target: %q", args)
		}
		switch args[0] {
		case "print":
			if !loaded {
				return nil, missing
			}
		case "bootout":
			loaded = false
		case "enable":
			disabled = false
		case "disable":
			disabled = true
		default:
			t.Fatalf("unexpected launchctl operation: %q", args)
		}
		return nil, nil
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Executable = "relative" }, func(c *Config) { c.StateDir = "relative" }} {
		bad := c
		mutate(&bad)
		if _, err := a.Install(t.Context(), bad); err == nil || len(calls) != 0 {
			t.Fatal("invalid install reached launchctl")
		}
	}
	first, err := a.Install(t.Context(), c)
	if err != nil || !first.Installed || !first.Loaded || !first.Changed || disabled {
		t.Fatalf("install: %+v %v", first, err)
	}
	before, _ := os.ReadFile(a.PlistPath)
	info, _ := os.Stat(a.PlistPath)
	for _, override := range []bool{false, true} {
		// Direct launchctl disable can leave the job loaded. Reinstall must
		// clear that override without replacing the plist or registration.
		disabled = override
		calls = nil
		second, err := a.Install(t.Context(), c)
		after, readErr := os.ReadFile(a.PlistPath)
		nextInfo, statErr := os.Stat(a.PlistPath)
		if err != nil || readErr != nil || statErr != nil || second.Changed || !second.Loaded || disabled || !bytes.Equal(before, after) || !os.SameFile(info, nextInfo) || !info.ModTime().Equal(nextInfo.ModTime()) || !reflect.DeepEqual(calls, []string{"print " + a.target(), "enable " + a.target(), "print " + a.target()}) {
			t.Fatalf("non-idempotent install (disabled=%t): %+v %v %q", override, second, errors.Join(err, readErr, statErr), calls)
		}
	}
	start := bytes.Index(before, []byte("<key>WatchPaths</key>"))
	if start < 0 {
		t.Fatal("rendered plist lacks WatchPaths")
	}
	end := bytes.Index(before[start:], []byte("</array>"))
	if end < 0 {
		t.Fatal("rendered WatchPaths is not an array")
	}
	end += start + len("</array>")
	legacy := append(append([]byte(nil), before[:start]...), before[end:]...)
	if err := os.WriteFile(a.PlistPath, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(c.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	calls = nil
	_, updateErr := a.Install(t.Context(), c)
	unchanged, readErr := os.ReadFile(a.PlistPath)
	closeErr := store.Close()
	if !errors.Is(updateErr, state.ErrLocked) || readErr != nil || closeErr != nil || !loaded || !bytes.Equal(legacy, unchanged) || !slices.Equal(calls, []string{"print " + a.target()}) {
		t.Fatalf("locked update interrupted the existing job: %v %q", errors.Join(updateErr, readErr, closeErr), calls)
	}
	logPath := filepath.Join(c.StateDir, autolog.OutName)
	if err := os.Chmod(logPath, 0644); err != nil {
		t.Fatal(err)
	}
	calls = nil
	_, updateErr = a.Install(t.Context(), c)
	unchanged, readErr = os.ReadFile(a.PlistPath)
	chmodErr := os.Chmod(logPath, 0600)
	if updateErr == nil || !strings.Contains(updateErr.Error(), "automatic log must be") || readErr != nil || chmodErr != nil || !loaded || !bytes.Equal(legacy, unchanged) || !slices.Equal(calls, []string{"print " + a.target()}) {
		t.Fatalf("failed log preparation interrupted the existing job: %v %q", errors.Join(updateErr, readErr, chmodErr), calls)
	}
	calls = nil
	upgraded, err := a.Install(t.Context(), c)
	after, readErr := os.ReadFile(a.PlistPath)
	if err != nil || readErr != nil || !upgraded.Changed || !upgraded.Loaded || !bytes.Equal(before, after) || !slices.Contains(calls, "bootout "+a.target()) || !slices.Contains(calls, "bootstrap "+a.Domain+" "+a.PlistPath) {
		t.Fatalf("WatchPaths upgrade: %+v %v %q", upgraded, errors.Join(err, readErr), calls)
	}
	if s, err := a.Disable(t.Context()); err != nil || s.Loaded || !s.Installed || !disabled {
		t.Fatalf("disable: %+v %v", s, err)
	}
	if s, err := a.Enable(t.Context()); err != nil || !s.Loaded || !s.Installed || disabled {
		t.Fatalf("enable: %+v %v", s, err)
	}
	retained := filepath.Join(c.StateDir, "retained")
	if err := os.WriteFile(retained, []byte("state and journal bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	// A direct launchctl disable can leave an already loaded job registered.
	disabled = true
	calls = nil
	if s, err := a.Uninstall(t.Context()); err != nil || s.Loaded || s.Installed {
		t.Fatalf("uninstall: %+v %v", s, err)
	}
	if enable, bootout := slices.Index(calls, "enable "+a.target()), slices.Index(calls, "bootout "+a.target()); disabled || enable < 0 || bootout <= enable || slices.Contains(calls, "disable "+a.target()) {
		t.Fatalf("uninstall left or created a disabled override: %q", calls)
	}
	if data, err := os.ReadFile(retained); err != nil || string(data) != "state and journal bytes" {
		t.Fatalf("state removed: %q %v", data, err)
	}
	if _, err := a.Uninstall(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Install(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Disable(t.Context()); err != nil || !disabled || loaded {
		t.Fatalf("disabled uninstall fixture: %v", err)
	}
	calls = nil
	if s, err := a.Uninstall(t.Context()); err != nil || s.Installed || s.Loaded || disabled || slices.Contains(calls, "disable "+a.target()) {
		t.Fatalf("uninstall of disabled job: %+v %v %q", s, err, calls)
	}
	loaded = true
	calls = nil
	if _, err := a.Install(t.Context(), c); err == nil || len(calls) != 1 {
		t.Fatalf("replaced foreign registered job: %v %q", err, calls)
	}
	if err := os.WriteFile(a.PlistPath, []byte("foreign plist"), 0600); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if _, err := a.Uninstall(t.Context()); err == nil || len(calls) != 0 {
		t.Fatalf("touched foreign plist: %v %q", err, calls)
	}
	a.launchctl = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("permission denied") }
	if _, err := a.loaded(t.Context()); err == nil {
		t.Fatal("print error treated as absence")
	}
}

func TestRexBinaryFallbackPlist(t *testing.T) {
	c := testConfig(t)
	pathDir := t.TempDir()
	if err := os.Symlink(c.Executable, filepath.Join(pathDir, "rex")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	c.Rex = filepath.Join(pathDir, "missing-configured-rex")
	a, err := New(c.Label, filepath.Join(t.TempDir(), "LaunchAgents"))
	if err != nil {
		t.Fatal(err)
	}
	absent := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 113").Run()
	loaded := false
	a.launchctl = func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case slices.Equal(args, []string{"print", a.target()}):
			if !loaded {
				return nil, absent
			}
		case slices.Equal(args, []string{"enable", a.target()}):
		case slices.Equal(args, []string{"bootstrap", a.Domain, a.PlistPath}):
			loaded = true
		default:
			t.Fatalf("unexpected launchctl operation: %q", args)
		}
		return nil, nil
	}
	missingResolved := c
	missingResolved.ResolvedRex = filepath.Join(pathDir, "removed-resolved-rex")
	if _, err := a.Install(t.Context(), missingResolved); !errors.Is(err, os.ErrNotExist) || loaded {
		t.Fatalf("re-resolved missing selected Rex: %v", err)
	}
	for _, changed := range []bool{true, false} {
		if !changed {
			c.ResolvedRex = c.Executable
		}
		status, err := a.Install(t.Context(), c)
		if err != nil || !status.Loaded || status.Changed != changed {
			t.Fatalf("install missing configured Rex: %+v %v", status, err)
		}
		data, err := os.ReadFile(a.PlistPath)
		if err != nil {
			t.Fatal(err)
		}
		values, err := parsePlist(data)
		if err != nil {
			t.Fatal(err)
		}
		args := values["ProgramArguments"].Strings
		i := slices.Index(args, "--rex")
		if i < 0 || i+1 >= len(args) || args[i+1] != c.Rex {
			t.Fatalf("configured Rex path changed: %q", args)
		}
	}
}

func TestLaunchctlOutputLimit(t *testing.T) {
	var output limitedOutput
	input := io.LimitReader(strings.NewReader(strings.Repeat("x", (1<<20)+1)), (1<<20)+1)
	if _, err := io.Copy(&output, input); err == nil || output.buf.Len() > 1<<20 {
		t.Fatalf("launchctl output was not bounded: %d bytes, %v", output.buf.Len(), err)
	}
}
