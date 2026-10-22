package rex

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

func waitDefaultTerminal(t *testing.T, c Client, session, block snapshot.LiveID, cwd string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), DefaultAutoWait)
	defer cancel()
	var process struct {
		Child struct {
			Path string `json:"path"`
			CWD  string `json:"cwd"`
		} `json:"child"`
		Foreground struct {
			Path string `json:"path"`
			CWD  string `json:"cwd"`
		} `json:"foreground"`
		LastExit json.RawMessage `json:"last_exit"`
	}
	var last error
	for ctx.Err() == nil {
		last = c.Call(ctx, "com.superlogical.terminal.process", map[string]any{"session_id": session, "block_id": block, "args": map[string]any{}}, &process)
		if last == nil && process.Child.Path == "/usr/bin/login" && process.Child.CWD == "" && process.Foreground.Path != "" {
			if resolved, err := snapshot.CanonicalCWD(process.Foreground.CWD); err == nil && resolved == cwd {
				t.Logf("default terminal %s: child=%s without cwd; foreground=%s cwd=%s", block, process.Child.Path, process.Foreground.Path, resolved)
				return
			}
		}
		if len(process.LastExit) != 0 && string(process.LastExit) != "null" {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("STOP: private default terminal %s did not launch login and a foreground shell in %s: process=%+v; call=%v; deadline=%v", block, cwd, process, last, ctx.Err())
}

func requireLoginShell(t *testing.T) {
	t.Helper()
	if os.Getenv("REXURRECT_SKIP_LOGIN_SHELL") == "1" {
		t.Skip("REXURRECT_SKIP_LOGIN_SHELL=1: the real login-shell test must run outside an agent sandbox")
	}
}

func TestDefaultTerminalRoundTrip(t *testing.T) {
	requireLoginShell(t)
	s := newIsolatedServer(t)
	sources := restoreFixture(t, s)
	cwd := filepath.Join(s.root, "normal terminal")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	var normal createdWindow
	mustCall(t, s.client, "session.new_window", map[string]any{"session_id": sources[0], "window_label": "normal", "layout": createNode{Block: &createBlock{Flavor: shellFlavor, Label: "normal", Options: shellOptions{CWD: cwd}}}, "focus": false}, &normal)
	if len(normal.Blocks) != 1 {
		t.Fatalf("default terminal creation: %+v", normal)
	}
	waitDefaultTerminal(t, s.client, sources[0], normal.Blocks[0], cwd)

	// Supply a known OSC 7 report through this test-owned terminal. This also
	// exercises URL escaping and the rule that the hostname is not authority.
	fileURL := (&url.URL{Scheme: "file", Host: "test-host.invalid", Path: cwd}).String()
	mustCall(t, s.client, "com.superlogical.terminal.write", map[string]any{"session_id": sources[0], "block_id": normal.Blocks[0], "args": map[string]any{"data": "printf '\\033]7;%s\\007' '" + fileURL + "'\n"}}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), DefaultAutoWait)
	defer cancel()
	for {
		var pwd struct {
			PWD string `json:"pwd"`
		}
		if err := s.client.Call(ctx, "com.superlogical.terminal.pwd", map[string]any{"session_id": sources[0], "block_id": normal.Blocks[0], "args": map[string]any{}}, &pwd); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(pwd.PWD)
		if err == nil && u.Scheme == "file" && u.Path == cwd {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("default terminal OSC 7 pwd=%q: %v", pwd.PWD, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	dir := filepath.Join(t.TempDir(), "saved")
	saved, err := s.client.Save(t.Context(), dir, false)
	if err != nil || saved.Generation != 1 {
		t.Fatalf("save normal terminals: %+v %v", saved, err)
	}
	firstPID := s.cmd.Process.Pid
	s.stop(t)
	s.start(t)
	if s.cmd.Process.Pid == firstPID || len(listSessions(t, s.client)) != 0 {
		t.Fatal("private restart did not produce a new empty server")
	}
	result, err := s.client.RestoreSaved(t.Context(), dir, saved.Generation)
	if err != nil || result.Outcome != "complete" || len(result.CWDSubstitutions) != 0 {
		t.Fatalf("restore normal terminals: %+v %v", result, err)
	}
	compareRestored(t, s.client, saved, result.IDMap)
	for _, session := range saved.Snapshot.Sessions {
		var check func(snapshot.Node)
		check = func(n snapshot.Node) {
			if n.Block != nil {
				waitDefaultTerminal(t, s.client, result.IDMap[session.ID], result.IDMap[n.Block.ID], n.Block.CWD)
				got, err := s.client.terminalCWD(t.Context(), result.IDMap[session.ID], result.IDMap[n.Block.ID])
				if err != nil || got != n.Block.CWD {
					t.Fatalf("restored cwd for %s: %q, want %q: %v", n.Block.ID, got, n.Block.CWD, err)
				}
				return
			}
			check(n.Split.Before)
			check(n.Split.After)
		}
		for _, w := range session.Windows {
			check(w.Layout)
		}
	}
	t.Logf("compared default-terminal restart under %d journal ID mappings", len(result.IDMap))
}
