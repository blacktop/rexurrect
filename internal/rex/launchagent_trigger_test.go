package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/autolog"
	"github.com/blacktop/rexurrect/internal/launchagent"
	"github.com/blacktop/rexurrect/internal/state"
)

func launchdPrint(t *testing.T, target string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/launchctl", "print", target).CombinedOutput()
	if err != nil {
		t.Fatalf("launchctl print: %v\n%s", err, out)
	}
	return string(out)
}

func launchdRuns(t *testing.T, job string) int {
	t.Helper()
	for line := range strings.SplitSeq(job, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "runs" {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("launchctl print has no run count: %s", job)
	return 0
}

// The startup trigger must wake the owned job through the ordinary launchd
// path, only when started by the configured server, and must never produce a
// second copy of a session when it races WatchPaths or a duplicate trigger.
func TestLaunchAgentStartupTrigger(t *testing.T) {
	if os.Getenv("REXURRECT_SKIP_LAUNCHD") == "1" {
		t.Skip("REXURRECT_SKIP_LAUNCHD=1: the real launchd test must run outside an agent sandbox")
	}
	if runtime.GOOS != "darwin" {
		t.Fatal("requires the macOS gui launchd domain; the real launchd test must run outside an agent sandbox")
	}
	s := newIsolatedServer(t)
	binary := filepath.Join(s.root, "rexurrect")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../cmd/rexurrect")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build LaunchAgent executable: %v\n%s", err, out)
	}
	dir, plistDir := filepath.Join(s.root, "saved"), filepath.Join(s.root, "LaunchAgents")
	label := "io.blacktop.rexurrect.test.trigger." + strconv.Itoa(os.Getpid()) + "." + filepath.Base(s.root)
	a, err := launchagent.New(label, plistDir)
	if err != nil {
		t.Fatal(err)
	}
	target := a.Domain + "/" + label
	if status, err := a.Status(t.Context()); err != nil || status.Installed || status.Loaded {
		t.Fatalf("test label is not available: %+v %v", status, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if _, err := a.Uninstall(ctx); err != nil {
			t.Errorf("agent cleanup: %v", err)
			if out, err := exec.CommandContext(ctx, "/bin/launchctl", "bootout", target).CombinedOutput(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 113 {
					t.Errorf("test-owned job cleanup: %v %s", err, out)
				}
			}
		}
		if status, err := a.Status(ctx); err != nil || status.Loaded {
			t.Errorf("test job remains registered: %+v %v", status, err)
		}
	})
	config := filepath.Join(s.root, "init.lua")
	generationWait := privateCycleTimeout + s.client.CallTimeout
	run := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = s.client.Env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%q: %v\n%s\n%s", args, err, &stdout, &stderr)
		}
		return stdout.Bytes()
	}
	agent := func(command string) launchagent.Status {
		t.Helper()
		// A day-long timer means every run below has another cause.
		out := run("agent", command, "--server", "unix://"+s.client.Socket, "--state-dir", dir, "--rex", s.client.Binary, "--label", label, "--plist-dir", plistDir, "--interval", "24h", "--wait", DefaultAutoWait.String(), "--timeout", privateCycleTimeout.String(), "--call-timeout", s.client.CallTimeout.String(), "--log-max-bytes", "4096", "--json")
		var status launchagent.Status
		if err := json.Unmarshal(out, &status); err != nil || status.Label != label {
			t.Fatalf("agent %s JSON: %s %v", command, out, err)
		}
		return status
	}
	waitRuns := func(want int, limit time.Duration) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for {
			job := launchdPrint(t, target)
			if got := launchdRuns(t, job); got >= want {
				if got != want {
					t.Fatalf("job ran %d times, want %d", got, want)
				}
				if strings.Contains(job, "state = not running") {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("job did not reach %d idle runs: %s", want, job)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// A running cycle holds the state lock, so readers retry until idle.
	current := func() state.Current {
		t.Helper()
		deadline := time.Now().Add(generationWait)
		for {
			store, err := state.Open(dir)
			if err == nil {
				c, loadErr := store.LoadCurrent()
				if err := errors.Join(loadErr, store.Close()); err != nil {
					out, _ := os.ReadFile(filepath.Join(dir, autolog.OutName))
					diagnostic, _ := os.ReadFile(filepath.Join(dir, autolog.ErrName))
					t.Fatalf("%v\nstdout: %s\nstderr: %s", err, out, diagnostic)
				}
				return c
			}
			if !errors.Is(err, state.ErrLocked) || time.Now().After(deadline) {
				out, _ := os.ReadFile(filepath.Join(dir, autolog.OutName))
				diagnostic, _ := os.ReadFile(filepath.Join(dir, autolog.ErrName))
				t.Fatalf("%v\nstdout: %s\nstderr: %s", err, out, diagnostic)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	sessions := func() int {
		t.Helper()
		ids, err := s.client.List(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return len(ids)
	}
	reload := func(lua []byte) {
		t.Helper()
		if err := os.WriteFile(config, lua, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.client.command(t.Context(), "config", "reload", "--json"); err != nil {
			t.Fatal(err)
		}
	}
	snippet := run("hook", "lua", "--label", label, "--plist-dir", plistDir)
	if !bytes.Contains(snippet, []byte(`"`+binary+`", "agent", "wake", "--label", "`+label+`", "--plist-dir", "`+plistDir+`"}`)) {
		t.Fatalf("hook lua:\n%s", snippet)
	}
	var created createdSession
	mustCall(t, s.client, "session.create", map[string]any{"label": "trigger-test", "initial_windows": []createWindow{{Label: "shell", Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}}}}, &created)
	if got := agent("install"); !got.Installed || !got.Loaded {
		t.Fatalf("install: %+v", got)
	}
	waitRuns(1, generationWait)
	if c := current(); c.Envelope.Generation != 1 || len(c.Envelope.Snapshot.Sessions) != 1 {
		t.Fatalf("RunAtLoad did not publish the fixture: generation %d", c.Envelope.Generation)
	}
	// Causality: a reload changes no socket and the timer is a day away, so a
	// second run within seconds can only come from the wake trigger.
	reload(snippet)
	waitRuns(2, 20*time.Second)
	if c := current(); c.Envelope.Generation != 1 {
		t.Fatalf("an unchanged same-instance wake published generation %d", c.Envelope.Generation)
	}
	t.Logf("config reload woke %s within the trigger window", target)
	// Without the dispatch, and under config check (whose child is reparented
	// before it runs), nothing wakes the job.
	reload(nil)
	if err := os.WriteFile(filepath.Join(s.root, "check.lua"), snippet, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := s.client.command(t.Context(), "config", "check", filepath.Join(s.root, "check.lua"), "--json"); err != nil {
		t.Fatalf("config check: %v\n%s", err, out)
	}
	time.Sleep(5 * time.Second)
	if got := launchdRuns(t, launchdPrint(t, target)); got != 2 {
		t.Fatalf("job ran without a dispatching trigger: %d runs", got)
	}
	// A direct launchctl disable leaves the service registered. Wake must
	// honor that override instead of running the still-loaded job.
	launchctl := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput(); err != nil {
			t.Fatalf("launchctl %q: %v\n%s", args, err, out)
		}
	}
	launchctl("disable", target)
	reload(snippet)
	time.Sleep(3 * time.Second)
	if got := launchdRuns(t, launchdPrint(t, target)); got != 2 {
		t.Fatalf("disabled but loaded job ran: %d runs", got)
	}
	launchctl("enable", target)
	// Changing only the on-disk configuration must not authorize the old
	// configuration that remains loaded under the same label.
	owned, err := os.ReadFile(a.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(owned, []byte("<string>"+dir+"</string>"), []byte("<string>"+dir+"-other</string>"), 1)
	if bytes.Equal(owned, changed) {
		t.Fatal("fixture state directory not found in plist")
	}
	if err := os.WriteFile(a.PlistPath, changed, 0600); err != nil {
		t.Fatal(err)
	}
	reload(snippet)
	time.Sleep(3 * time.Second)
	if got := launchdRuns(t, launchdPrint(t, target)); got != 2 {
		t.Fatalf("mismatched loaded job ran: %d runs", got)
	}
	if err := os.WriteFile(a.PlistPath, owned, 0600); err != nil {
		t.Fatal(err)
	}
	// A state lock held elsewhere turns the woken cycle into a benign miss.
	held, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reload(snippet)
	waitRuns(3, 20*time.Second)
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if diagnostic, err := os.ReadFile(filepath.Join(dir, autolog.ErrName)); err != nil || !bytes.Contains(diagnostic, []byte("Automatic cycle locked")) {
		t.Fatalf("locked wake was not reported: %v\n%s", err, diagnostic)
	}
	// Disabled means unloaded: the trigger must not bootstrap or run it.
	if got := agent("disable"); got.Loaded {
		t.Fatalf("disable: %+v", got)
	}
	reload(snippet)
	time.Sleep(5 * time.Second)
	if got, err := a.Status(t.Context()); err != nil || got.Loaded {
		t.Fatalf("a disabled job was woken: %+v %v", got, err)
	}
	if got := agent("enable"); !got.Loaded {
		t.Fatalf("enable: %+v", got)
	}
	waitRuns(1, generationWait)
	// Prove a real restore is caused by startup dispatch alone. Suppress
	// both other immediate launch conditions; the remaining timer is 24h.
	if got := agent("disable"); got.Loaded {
		t.Fatalf("disable: %+v", got)
	}
	for _, key := range []string{"WatchPaths", "RunAtLoad"} {
		if out, err := exec.CommandContext(t.Context(), "/usr/bin/plutil", "-remove", key, a.PlistPath).CombinedOutput(); err != nil {
			t.Fatalf("remove %s: %v\n%s", key, err, out)
		}
	}
	if got := agent("enable"); !got.Loaded {
		t.Fatalf("enable: %+v", got)
	}
	reload(nil)
	s.stop(t)
	s.start(t)
	time.Sleep(3 * time.Second)
	if got := launchdRuns(t, launchdPrint(t, target)); got != 0 || sessions() != 0 || current().Envelope.Generation != 1 {
		t.Fatalf("restart without a trigger caused recovery: %d runs", got)
	}
	if err := os.WriteFile(config, snippet, 0600); err != nil {
		t.Fatal(err)
	}
	s.stop(t)
	s.start(t)
	waitRuns(1, generationWait)
	if after := current(); after.Envelope.Generation != 2 || len(after.Envelope.Snapshot.Sessions) != 1 || sessions() != 1 {
		t.Fatalf("startup alone did not restore the fixture: %+v", after.Envelope)
	}
	// Restore the normal timer/WatchPaths job for the duplicate-trigger race.
	if got := agent("install"); !got.Changed || !got.Loaded {
		t.Fatalf("reinstall: %+v", got)
	}
	waitRuns(1, generationWait)
	// Duplicate triggers race WatchPaths across a real server restart; the
	// state lock and journal must still yield exactly one restored copy.
	if err := os.WriteFile(config, append(append([]byte(nil), snippet...), snippet...), 0600); err != nil {
		t.Fatal(err)
	}
	before := current()
	s.stop(t)
	s.start(t)
	deadline := time.Now().Add(generationWait)
	for current().Envelope.Generation < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("restart was not recovered: %+v", current().Envelope)
		}
		time.Sleep(100 * time.Millisecond)
	}
	after := current()
	if after.Envelope.InstanceID == before.Envelope.InstanceID || after.Envelope.Generation != 3 || len(after.Envelope.Snapshot.Sessions) != 1 {
		t.Fatalf("restart recovery: %+v", after.Envelope)
	}
	// The published generation's own journal is fresh; the restore that
	// preceded it is reported by the woken cycle.
	out, err := os.ReadFile(filepath.Join(dir, autolog.OutName))
	if err != nil || !bytes.Contains(out, []byte(`"state":"new_instance"`)) || !bytes.Contains(out, []byte(`"restore_outcome":"complete"`)) || !bytes.Contains(out, []byte(`"saved_generation":3`)) {
		t.Fatalf("restart cycle report: %v\n%s", err, out)
	}
	for range 3 {
		if n := sessions(); n != 1 {
			t.Fatalf("restored session was duplicated: %d sessions", n)
		}
		time.Sleep(time.Second)
	}
	t.Logf("restart recovered one session with %d launchd runs", launchdRuns(t, launchdPrint(t, target)))
}
