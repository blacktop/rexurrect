package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

type isolatedServer struct {
	client  Client
	root    string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	done    chan struct{}
	waitErr error
	stderr  bytes.Buffer
	// restore lets the private server bring its own sessions back from its
	// snapshot on restart, as the live app server does. Off by default so
	// the restart tests keep an empty baseline.
	restore bool
}

const privateCycleTimeout = DefaultAutoTimeout

func newIsolatedServer(t *testing.T) *isolatedServer {
	t.Helper()
	return newIsolatedServerWithSetup(t, nil)
}

func newIsolatedServerWithSetup(t *testing.T, setup func(*isolatedServer)) *isolatedServer {
	t.Helper()
	binary, err := exec.LookPath("rex")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/private/tmp", "rr-")
	if err != nil {
		t.Fatal(err)
	}
	s := &isolatedServer{root: root, client: Client{Binary: binary, Socket: filepath.Join(root, "s"), CallTimeout: DefaultCallTimeout}}
	s.client.Env = []string{"HOME=" + filepath.Join(root, "home"), "XDG_CONFIG_HOME=" + filepath.Join(root, "home", "config"), "XDG_DATA_HOME=" + filepath.Join(root, "home", "data"), "XDG_STATE_HOME=" + filepath.Join(root, "home", "state"), "XDG_CACHE_HOME=" + filepath.Join(root, "home", "cache"), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "SHELL=/bin/sh", "TERM=xterm-256color", "TMPDIR=" + root}
	t.Cleanup(func() {
		if s.cmd != nil {
			ctx, cancel := context.WithTimeout(context.Background(), privateCycleTimeout)
			ids, err := s.client.List(ctx)
			if err != nil {
				t.Errorf("list private cleanup sessions: %v", err)
			}
			for _, id := range ids {
				if err := s.client.Call(ctx, "session.destroy", map[string]any{"session_id": id}, nil); err != nil {
					t.Errorf("destroy private session: %v", err)
				}
			}
			cancel()
			s.stop(t)
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"home", "state"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "init.lua"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(s)
	}
	s.start(t)
	return s
}

func (s *isolatedServer) start(t *testing.T) {
	t.Helper()
	if err := s.tryStart(t); err != nil {
		t.Fatalf("private server failed to start: %v", err)
	}
}

// died reports whether the private server exited on its own and reaps it
// without failing the test. Rex's native restore can crash the restarted
// server, which the native tests retry rather than report as ours.
func (s *isolatedServer) died() (error, bool) {
	if s.cmd == nil {
		return nil, false
	}
	select {
	case <-s.done:
		err := fmt.Errorf("private server exited: %w\n%s", s.waitErr, s.stderr.String())
		s.cancel()
		s.cmd = nil
		return err, true
	default:
		return nil, false
	}
}

func (s *isolatedServer) tryStart(t *testing.T) error {
	t.Helper()
	if s.cmd != nil {
		t.Fatal("server already running")
	}
	// Keep the owned server alive through several bounded cycles and the
	// LaunchAgent's interval checks, even when the host is busy at login.
	ctx, cancel := context.WithTimeout(context.Background(), 6*privateCycleTimeout)
	s.cancel = cancel
	s.cmd = exec.CommandContext(ctx, s.client.Binary, "--autostart=false", "server", "run", "--listen", "unix://"+s.client.Socket, "--state-dir", filepath.Join(s.root, "state"), "--config", filepath.Join(s.root, "init.lua"), "--restore="+strconv.FormatBool(s.restore), "--max-sessions", "4", "--max-blocks", "32", "--max-windows", "4", "--max-panes", "8")
	s.cmd.Env = s.client.Env
	s.stderr.Reset()
	s.cmd.Stderr = &s.stderr
	if err := s.cmd.Start(); err != nil {
		s.cmd = nil
		cancel()
		t.Fatal(err)
	}
	s.done = make(chan struct{})
	go func() { s.waitErr = s.cmd.Wait(); close(s.done) }()
	ready, stop := context.WithTimeout(t.Context(), DefaultAutoWait)
	defer stop()
	var last error
	for ready.Err() == nil {
		if _, last = s.client.List(ready); last == nil {
			t.Logf("private Rex PID %d ready", s.cmd.Process.Pid)
			return nil
		}
		if err, dead := s.died(); dead {
			return err
		}
		select {
		case <-ready.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
	return last
}

func (s *isolatedServer) stop(t *testing.T) {
	t.Helper()
	if s.cmd == nil {
		return
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("signal private server: %v", err)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			t.Fatal("private server did not exit")
		}
	}
	s.cancel()
	if s.waitErr != nil {
		t.Errorf("private server exit: %v\n%s", s.waitErr, s.stderr.String())
	}
	s.cmd = nil
}

func mustCall(t *testing.T, c Client, method string, args, result any) {
	t.Helper()
	if err := c.Call(t.Context(), method, args, result); err != nil {
		t.Fatal(err)
	}
}

func TestIsolatedRoundTrip(t *testing.T) {
	requireLoginShell(t)
	s := newIsolatedServer(t)
	version, err := s.client.command(t.Context(), "version")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(strings.TrimSpace(string(version)))
	if ids, err := s.client.List(t.Context()); err != nil || len(ids) != 0 {
		t.Fatalf("initial private baseline: %v %v", ids, err)
	}
	dirs := make([]string, 5)
	for i, name := range []string{"first", "left", "top", "bottom", "changed"} {
		dirs[i] = filepath.Join(s.root, name)
		if err := os.Mkdir(dirs[i], 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(s.root, "alias")
	if err := os.Symlink(dirs[1], alias); err != nil {
		t.Fatal(err)
	}
	leaf := func(label, cwd string) createNode {
		return createNode{Block: &createBlock{Flavor: shellFlavor, Label: label, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: cwd}}}
	}
	nested := createNode{Split: &createSplit{Direction: "horizontal", Ratio: 0.37, Before: leaf("left", alias), After: createNode{Split: &createSplit{Direction: "vertical", Ratio: 0.61, Before: leaf("top", dirs[2]), After: leaf("bottom", dirs[3])}}}}
	var sources []snapshot.LiveID
	var created []createdSession
	for range 2 {
		var result createdSession
		mustCall(t, s.client, "session.create", map[string]any{"label": "same-session-label", "initial_windows": []createWindow{{Label: "first", Layout: leaf("first", dirs[0])}, {Label: "work", Layout: nested}}}, &result)
		if len(result.Windows) != 2 || len(result.Windows[0].Blocks) != 1 || len(result.Windows[1].Blocks) != 3 {
			t.Fatalf("fixture creation inventory: %+v", result)
		}
		sources = append(sources, result.ID)
		created = append(created, result)
		mustCall(t, s.client, "session.focus_block", map[string]any{"session_id": result.ID, "block_id": result.Windows[1].Blocks[2]}, nil)
		mustCall(t, s.client, "session.zoom_block", map[string]any{"session_id": result.ID, "block_id": result.Windows[1].Blocks[2], "zoom": true}, nil)
	}
	// Change the running shell's cwd after creation; no command is saved or replayed.
	changedBlock := created[0].Windows[1].Blocks[2]
	mustCall(t, s.client, "com.superlogical.terminal.write", map[string]any{"session_id": sources[0], "block_id": changedBlock, "args": map[string]any{"data": "cd '" + dirs[4] + "'\n"}}, nil)
	deadline := time.Now().Add(privateCycleTimeout)
	for {
		var process struct {
			Child *struct {
				CWD string `json:"cwd"`
			} `json:"child"`
		}
		mustCall(t, s.client, "com.superlogical.terminal.process", map[string]any{"session_id": sources[0], "block_id": changedBlock, "args": map[string]any{}}, &process)
		if process.Child != nil && process.Child.CWD == dirs[4] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shell cwd did not change")
		}
		time.Sleep(25 * time.Millisecond)
	}
	var floating struct {
		ID     snapshot.LiveID   `json:"layer_id"`
		Blocks []snapshot.LiveID `json:"block_ids"`
	}
	mustCall(t, s.client, "session.new_layer", map[string]any{"session_id": sources[0], "window_id": created[0].Windows[0].ID, "bounds": []float64{0.1, 0.1, 0.6, 0.5}, "layout": leaf("floating", dirs[0]), "focus": false}, &floating)
	var detached struct {
		ID snapshot.LiveID `json:"block_id"`
	}
	mustCall(t, s.client, "session.new_block", map[string]any{"session_id": sources[1], "flavor": shellFlavor, "label": "detached", "options": shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: dirs[0]}}, &detached)
	if detached.ID == "" {
		t.Fatal("detached fixture has no block ID")
	}
	before, report, err := s.client.Capture(t.Context(), sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unsupported) != 2 || report.Unsupported[0].LayerID != snapshot.SourceID(floating.ID) || report.Unsupported[0].Kind != "floating" || len(floating.Blocks) != 1 || !reflect.DeepEqual(report.Unsupported[0].BlockIDs, []snapshot.SourceID{snapshot.SourceID(floating.Blocks[0])}) {
		t.Fatalf("floating layer was not explicitly reported: %+v", report)
	}
	wantDetached := snapshot.Unsupported{SessionID: snapshot.SourceID(sources[1]), Kind: "detached", BlockIDs: []snapshot.SourceID{snapshot.SourceID(detached.ID)}, Flavor: shellFlavor, Reason: "detached block is absent from window layouts"}
	if !reflect.DeepEqual(report.Unsupported[1], wantDetached) {
		t.Fatalf("detached block %s was not explicitly reported: %+v", detached.ID, report)
	}
	inventory, err := before.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inventory[snapshot.SourceID(detached.ID)]; ok {
		t.Fatal("capture claimed unsupported detached block")
	}
	if len(before.Sessions) != 2 {
		t.Fatal("capture omitted a session")
	}
	for i, session := range before.Sessions {
		if session.ID != snapshot.SourceID(sources[i]) || session.Label != "same-session-label" || len(session.Windows) != 2 || session.ActiveWindow != snapshot.SourceID(created[i].Windows[1].ID) {
			t.Fatalf("session capture differs: %+v", session)
		}
		w := session.Windows[1]
		if session.Windows[0].Label != "first" || w.Label != "work" || w.Focus != snapshot.SourceID(created[i].Windows[1].Blocks[2]) || w.Zoom != w.Focus {
			t.Fatal("window order, label, focus, or zoom differs")
		}
		p := w.Layout.Split
		if p == nil || p.Direction != "horizontal" || p.Ratio != 0.37 || p.After.Split == nil || p.After.Split.Direction != "vertical" || p.After.Split.Ratio != 0.61 {
			t.Fatal("nested split differs")
		}
		last := dirs[3]
		if i == 0 {
			last = dirs[4]
		}
		blocks := []*snapshot.Block{session.Windows[0].Layout.Block, p.Before.Block, p.After.Split.Before.Block, p.After.Split.After.Block}
		for j, cwd := range []string{dirs[0], dirs[1], dirs[2], last} {
			if blocks[j] == nil || blocks[j].CWD != cwd {
				t.Fatalf("process cwd %d/%d differs: %+v", i, j, blocks[j])
			}
		}
	}
	encoded, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var saved snapshot.Snapshot
	if err := json.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	firstPID := s.cmd.Process.Pid
	s.stop(t)
	s.start(t)
	if s.cmd.Process.Pid == firstPID {
		t.Fatal("server PID did not change")
	}
	if ids, err := s.client.List(t.Context()); err != nil || len(ids) != 0 {
		t.Fatalf("native restart baseline is not empty: %v %v", ids, err)
	}
	t.Log("same-state-directory native restart baseline: zero sessions")
	ids, err := s.client.Restore(t.Context(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ids[snapshot.SourceID(detached.ID)]; ok {
		t.Fatal("restore claimed unsupported detached block")
	}
	if _, ok := ids[snapshot.SourceID(floating.ID)]; ok {
		t.Fatal("restore claimed unsupported layer")
	}
	for _, id := range floating.Blocks {
		if _, ok := ids[snapshot.SourceID(id)]; ok {
			t.Fatal("restore claimed unsupported block")
		}
	}
	var targets []snapshot.LiveID
	for _, session := range saved.Sessions {
		targets = append(targets, ids[session.ID])
	}
	live, err := s.client.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != len(targets) {
		t.Fatalf("restored session count: %d", len(live))
	}
	for old, newID := range ids {
		if snapshot.LiveID(old) == newID {
			t.Fatalf("source ID reused: %s", old)
		}
	}
	after, restoredReport, err := s.client.Capture(t.Context(), targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(restoredReport.Unsupported) != 0 {
		t.Fatalf("unexpected restored unsupported state: %+v", restoredReport)
	}
	if err := snapshot.Compare(saved, after, ids); err != nil {
		t.Fatal(err)
	}
	t.Logf("compared %d sessions under %d explicit ID mappings; floating layer and detached block %s reported and excluded", len(after.Sessions), len(ids), detached.ID)
}
