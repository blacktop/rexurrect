package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/rex"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

func TestAutoBinaryPrivateServer(t *testing.T) {
	const cycleTimeout = rex.DefaultAutoTimeout
	rexBinary, err := exec.LookPath("rex")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/private/tmp", "rr-auto-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	binary := filepath.Join(root, "rexurrect")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build auto binary: %v\n%s", err, output)
	}
	for _, name := range []string{"home", "state"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(root, "init.lua")
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	client := rex.Client{Binary: rexBinary, Socket: filepath.Join(root, "s"), Env: []string{"HOME=" + filepath.Join(root, "home"), "XDG_CONFIG_HOME=" + filepath.Join(root, "home", "config"), "XDG_DATA_HOME=" + filepath.Join(root, "home", "data"), "XDG_STATE_HOME=" + filepath.Join(root, "home", "state"), "XDG_CACHE_HOME=" + filepath.Join(root, "home", "cache"), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "SHELL=/bin/sh", "TERM=xterm-256color", "TMPDIR=" + root}}
	client.CallTimeout = rex.DefaultCallTimeout
	ctx, cancel := context.WithTimeout(context.Background(), 3*cycleTimeout)
	server := exec.CommandContext(ctx, rexBinary, "--autostart=false", "server", "run", "--listen", "unix://"+client.Socket, "--state-dir", filepath.Join(root, "state"), "--config", config, "--max-sessions", "2", "--max-blocks", "4", "--max-windows", "2", "--max-panes", "2")
	server.Env = client.Env
	if err := server.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	t.Cleanup(func() {
		defer cancel()
		cleanup, stop := context.WithTimeout(context.Background(), cycleTimeout)
		defer stop()
		ids, err := client.List(cleanup)
		if err != nil {
			t.Errorf("private cleanup list: %v", err)
		}
		for _, id := range ids {
			if err := client.Call(cleanup, "session.destroy", map[string]any{"session_id": id}, nil); err != nil {
				t.Errorf("private cleanup session: %v", err)
			}
		}
		if err := server.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("private server exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			cancel()
			if err := <-done; err != nil {
				t.Errorf("private server forced exit: %v", err)
			}
		}
	})
	ready, stopReady := context.WithTimeout(t.Context(), rex.DefaultAutoWait)
	defer stopReady()
	for {
		if _, err := client.List(ready); err == nil {
			break
		}
		select {
		case <-ready.Done():
			t.Fatalf("private server readiness: %v", ready.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	var created struct {
		ID snapshot.LiveID `json:"session_id"`
	}
	if err := client.Call(t.Context(), "session.create", map[string]any{"label": "binary-test", "initial_windows": []any{map[string]any{"window_label": "shell", "layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"shell": "/bin/sh", "login": false, "cwd": root}}}}}}, &created); err != nil {
		t.Fatal(err)
	}
	if !t.Run("default-terminal", func(t *testing.T) {
		if os.Getenv("REXURRECT_SKIP_LOGIN_SHELL") == "1" {
			t.Skip("REXURRECT_SKIP_LOGIN_SHELL=1: the real login-shell test must run outside an agent sandbox")
		}
		var window struct {
			Blocks []snapshot.LiveID `json:"block_ids"`
		}
		if err := client.Call(t.Context(), "session.new_window", map[string]any{"session_id": created.ID, "window_label": "normal", "layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"cwd": root}}}}, &window); err != nil {
			t.Fatal(err)
		}
		if len(window.Blocks) != 1 {
			t.Fatalf("default terminal creation: %+v", window)
		}
		ctx, cancel := context.WithTimeout(t.Context(), rex.DefaultAutoWait)
		defer cancel()
		for {
			var process struct {
				Child struct {
					Path string `json:"path"`
					CWD  string `json:"cwd"`
				} `json:"child"`
				Foreground struct {
					CWD string `json:"cwd"`
				} `json:"foreground"`
				LastExit json.RawMessage `json:"last_exit"`
			}
			if err := client.Call(ctx, "com.superlogical.terminal.process", map[string]any{"session_id": created.ID, "block_id": window.Blocks[0], "args": map[string]any{}}, &process); err != nil {
				t.Fatal(err)
			}
			if process.Child.Path == "/usr/bin/login" && process.Child.CWD == "" {
				if cwd, err := snapshot.CanonicalCWD(process.Foreground.CWD); err == nil && cwd == root {
					return
				}
			}
			if len(process.LastExit) != 0 && string(process.LastExit) != "null" {
				t.Fatalf("STOP: private default terminal exited: %s", process.LastExit)
			}
			select {
			case <-ctx.Done():
				t.Fatalf("default terminal %s never became ready: %+v: %v", window.Blocks[0], process, ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
	}) {
		t.FailNow()
	}

	runAuto := func(socket, wait, want string, generation, saved uint64, success bool) rex.AutoResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), cycleTimeout+client.CallTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "auto", "--server", "unix://"+socket, "--state-dir", filepath.Join(root, "saved"), "--rex", rexBinary, "--wait", wait, "--timeout", cycleTimeout.String(), "--call-timeout", client.CallTimeout.String(), "--json")
		cmd.Env = client.Env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		if success && runErr != nil || !success && (runErr == nil || cmd.ProcessState.ExitCode() == 0) {
			t.Fatalf("auto exit: %v\n%s", runErr, &stderr)
		}
		var result rex.AutoResult
		decoder := json.NewDecoder(&stdout)
		if err := decoder.Decode(&result); err != nil {
			t.Fatalf("decode auto: %v\n%s", err, &stdout)
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			t.Fatalf("extra stdout: %v", err)
		}
		wantReason := ""
		if success && saved == 0 {
			wantReason = "unchanged"
		}
		if result.State != want || result.Generation != generation || result.SavedGeneration != saved || success && result.Reason != wantReason || !success && result.Reason == "" || strings.TrimSpace(stderr.String()) == "" {
			t.Fatalf("auto result: %+v\n%s", result, &stderr)
		}
		return result
	}
	first := runAuto(client.Socket, rex.DefaultAutoWait.String(), "first_run", 0, 1, true)
	if first.InstanceID == "" || first.RestoreOutcome != "not_started" {
		t.Fatalf("first-run identity: %+v", first)
	}
	second := runAuto(client.Socket, rex.DefaultAutoWait.String(), "same_instance", 1, 0, true)
	if second.InstanceID != first.InstanceID || second.RestoreOutcome != "not_started" {
		t.Fatalf("same-instance identity: %+v", second)
	}
	if ids, err := client.List(t.Context()); err != nil || len(ids) != 1 || ids[0] != created.ID {
		t.Fatalf("binary duplicated the session: %v %v", ids, err)
	}
	runAuto(filepath.Join(root, "missing"), "100ms", "unavailable", 1, 0, false)

	// A manual save whose scrollback capture degrades still succeeds, and
	// its human output names every skipped block and the reason.
	saveCtx, stopSave := context.WithTimeout(t.Context(), cycleTimeout)
	defer stopSave()
	save := exec.CommandContext(saveCtx, binary, "save", "--server", "unix://"+client.Socket, "--state-dir", filepath.Join(root, "saved"), "--rex", rexBinary, "--call-timeout", client.CallTimeout.String(), "--scrollback")
	save.Env = append(slices.Clone(client.Env), "REXURRECT_SECURITY_TOOL="+filepath.Join(root, "missing-security"))
	var saveOut, saveErr bytes.Buffer
	save.Stdout, save.Stderr = &saveOut, &saveErr
	if err := save.Run(); err != nil {
		t.Fatalf("save --scrollback: %v\n%s%s", err, &saveOut, &saveErr)
	}
	if out := saveOut.String(); !strings.Contains(out, "Saved generation 2 ") || strings.Count(out, ": scrollback not captured (scrollback key: ") != 2 {
		t.Fatalf("save output does not report the two skipped blocks:\n%s", out)
	}
	t.Logf("compiled auto saved generation 1, skipped an unchanged capture on private Rex PID %d, returned a structured nonzero unavailable outcome, and manual save reported its scrollback skips", server.Process.Pid)
}
