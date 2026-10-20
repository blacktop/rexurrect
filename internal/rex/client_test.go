package rex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func scriptClient(t *testing.T, script string, env ...string) Client {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "rex-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return Client{Binary: binary, Socket: "/private/tmp/rex-client-test-unused", Env: env}
}

func TestCallRejectsInvalidMethod(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "invoked")
	c := scriptClient(t, "printf 'invoked\\n' > \"$MARKER\"\nprintf '{}\\n'\n", "MARKER="+marker)
	for _, method := range []string{"--help", "", ".list", "_list", "1list", "Session.list", "session-list", "session/list", "session list", "session.é", "session.list\n"} {
		if err := c.Call(t.Context(), method, nil, nil); err == nil || !strings.Contains(err.Error(), "invalid Rex API method") {
			t.Errorf("method %q: %v", method, err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid method invoked the command: %v", err)
	}
	var result struct{}
	if err := c.Call(t.Context(), "session.list_2", nil, &result); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("valid method did not invoke the command: %v", err)
	}
}

func TestCallTimeoutBounds(t *testing.T) {
	c := scriptClient(t, `printf '%s\n' "$5"`)
	for _, timeout := range []time.Duration{0, DefaultCallTimeout, MaxCallTimeout} {
		c.CallTimeout = timeout
		data, err := c.command(t.Context(), "version")
		want := timeout
		if want == 0 {
			want = DefaultCallTimeout
		}
		if err != nil || strings.TrimSpace(string(data)) != want.String() {
			t.Errorf("timeout %s: %q %v", timeout, data, err)
		}
	}
	for _, timeout := range []time.Duration{-time.Second, MaxCallTimeout + time.Nanosecond} {
		c.CallTimeout = timeout
		if _, err := c.command(t.Context(), "version"); err == nil || !strings.Contains(err.Error(), "--call-timeout") {
			t.Errorf("invalid timeout %s: %v", timeout, err)
		}
	}
}

func TestCallCancellationClosesInheritedPipes(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	c := scriptClient(t, "/bin/sleep 10 &\nprintf '%s %s\\n' \"$$\" \"$!\" > \"$PIDS\"\nwait\n", "PIDS="+pidFile)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := time.Now()
	var callErr error
	pprof.Do(ctx, pprof.Labels("rex-client-cancellation", t.Name()), func(ctx context.Context) {
		callErr = c.Call(ctx, "session.list", nil, nil)
	})
	elapsed := time.Since(started)

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		t.Fatalf("invalid fixture process IDs: %q", data)
	}
	childPID, err := strconv.Atoi(fields[0])
	if err != nil || childPID <= 1 {
		t.Fatalf("invalid child PID: %q", fields[0])
	}
	descendantPID, err := strconv.Atoi(fields[1])
	if err != nil || descendantPID <= 1 {
		t.Fatalf("invalid descendant PID: %q", fields[1])
	}
	descendant, err := os.FindProcess(descendantPID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := descendant.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill fixture descendant: %v", err)
		}
		if err := descendant.Release(); err != nil {
			t.Errorf("release fixture descendant: %v", err)
		}
	}()
	if elapsed >= 3*time.Second {
		t.Errorf("Call took %s after a one-second deadline", elapsed)
	}
	if !errors.Is(callErr, context.DeadlineExceeded) {
		t.Errorf("Call did not report its deadline: %v", callErr)
	}
	var exitErr *exec.ExitError
	if !errors.As(callErr, &exitErr) || exitErr.Pid() != childPID {
		t.Errorf("Call did not wait for child %d: %v", childPID, callErr)
	}
	if _, err := syscall.Wait4(childPID, nil, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Errorf("child %d was not already reaped: %v", childPID, err)
	}
	if err := descendant.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("descendant did not keep its inherited pipes open: %v", err)
	}

	// Labels are inherited by the command's reader goroutines. Allow a sender
	// that just delivered its result to finish, then require all of them to exit.
	deadline := time.Now().Add(time.Second)
	for {
		var goroutines bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&goroutines, 1); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(goroutines.String(), `"rex-client-cancellation":`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("command goroutine leaked:\n%s", goroutines.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
