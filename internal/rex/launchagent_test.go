package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/autolog"
	"github.com/blacktop/rexurrect/internal/launchagent"
	"github.com/blacktop/rexurrect/internal/state"
)

func TestLaunchAgentLifecycle(t *testing.T) {
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
	label := "io.blacktop.rexurrect.test." + strconv.Itoa(os.Getpid()) + "." + filepath.Base(s.root)
	a, err := launchagent.New(label, plistDir)
	if err != nil {
		t.Fatal(err)
	}
	target := a.Domain + "/" + label
	if status, err := a.Status(t.Context()); err != nil || status.Installed || status.Loaded {
		t.Fatalf("test label is not available: %+v %v", status, err)
	}
	// From this point the exact PID-suffixed label belongs to this test. Remove
	// it even if installation or a later assertion fails partway through.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if _, err := a.Uninstall(ctx); err != nil {
			t.Errorf("agent cleanup: %v", err)
			if out, err := exec.CommandContext(ctx, "/bin/launchctl", "enable", target).CombinedOutput(); err != nil {
				t.Errorf("test-owned override cleanup: %v %s", err, out)
			}
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
	var created createdSession
	mustCall(t, s.client, "session.create", map[string]any{"label": "launchagent-test", "initial_windows": []createWindow{{Label: "shell", Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}}}}, &created)
	const interval = time.Minute
	agentInterval := interval
	generationWait := privateCycleTimeout + s.client.CallTimeout
	base := []string{"--server", "unix://" + s.client.Socket, "--state-dir", dir, "--rex", s.client.Binary, "--label", label, "--plist-dir", plistDir, "--wait", DefaultAutoWait.String(), "--timeout", privateCycleTimeout.String(), "--call-timeout", s.client.CallTimeout.String(), "--log-max-bytes", "1024", "--json"}
	run := func(command string) launchagent.Status {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		args := append([]string{"agent", command, "--interval", agentInterval.String()}, base...)
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = s.client.Env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("agent %s: %v\n%s\n%s", command, err, &stdout, &stderr)
		}
		var status launchagent.Status
		if err := json.Unmarshal(stdout.Bytes(), &status); err != nil || status.Label != label || status.Domain != a.Domain {
			t.Fatalf("agent %s JSON: %s %v", command, &stdout, err)
		}
		return status
	}
	waitGeneration := func(want uint64, limit time.Duration) state.Current {
		t.Helper()
		deadline := time.Now().Add(limit)
		var last error
		for time.Now().Before(deadline) {
			store, err := state.Open(dir)
			if err == nil {
				current, loadErr := store.LoadCurrent()
				err = errors.Join(loadErr, store.Close())
				if err == nil && current.Envelope.Generation >= want {
					if current.Envelope.Generation != want {
						t.Fatalf("generation skipped: got %d, want %d", current.Envelope.Generation, want)
					}
					return current
				}
			}
			last = err
			time.Sleep(100 * time.Millisecond)
		}
		out, _ := os.ReadFile(filepath.Join(dir, autolog.OutName))
		diagnostic, _ := os.ReadFile(filepath.Join(dir, autolog.ErrName))
		t.Fatalf("generation %d was not published: %v\nstdout: %s\nstderr: %s", want, last, out, diagnostic)
		return state.Current{}
	}
	printJob := func() string { return launchdPrint(t, target) }
	runs := func(job string) int { return launchdRuns(t, job) }
	waitIdle := func() int {
		t.Helper()
		deadline := time.Now().Add(generationWait)
		for {
			job := printJob()
			if strings.Contains(job, "state = not running") && runs(job) > 0 {
				return runs(job)
			}
			if time.Now().After(deadline) {
				t.Fatalf("job did not finish its cycle: %s", job)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	checkWatchPaths := func() {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "/usr/bin/plutil", "-extract", "WatchPaths", "json", "-o", "-", a.PlistPath).CombinedOutput()
		var paths []string
		if err != nil || json.Unmarshal(out, &paths) != nil || !slices.Equal(paths, []string{s.client.Socket}) {
			t.Fatalf("WatchPaths must contain only the configured socket: %s %v", out, err)
		}
	}
	jobDisabled := func() bool {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		domain, err := exec.CommandContext(ctx, "/bin/launchctl", "print", a.Domain).CombinedOutput()
		if err != nil {
			t.Fatalf("launchctl print GUI domain: %v", err)
		}
		if !bytes.Contains(domain, []byte("disabled services = {")) {
			t.Fatal("launchctl print GUI domain did not expose its disabled-services section")
		}
		for line := range strings.SplitSeq(string(domain), "\n") {
			key, value, ok := strings.Cut(line, "=>")
			if ok && strings.TrimSpace(key) == strconv.Quote(label) {
				return strings.TrimSpace(value) == "disabled" || strings.TrimSpace(value) == "true"
			}
		}
		return false
	}
	if got := run("install"); !got.Installed || !got.Loaded || !got.Changed {
		t.Fatalf("first install: %+v", got)
	}
	checkWatchPaths()
	first := waitGeneration(1, generationWait)
	if len(first.Envelope.Snapshot.Sessions) != 1 || !strings.Contains(printJob(), binary) {
		t.Fatal("RunAtLoad did not capture the private fixture through the compiled binary")
	}
	t.Logf("RunAtLoad published generation 1 through %s", target)
	// Give the timer an observable edit to save; unchanged cycles publish nothing.
	mustCall(t, s.client, "session.set_label", map[string]any{"session_id": created.ID, "label": "timer-edit"}, nil)
	// Prove the configured StartInterval actually fires. Seeing two runs also
	// lets print detect an accidental bootout/bootstrap on the identical install.
	waitGeneration(2, interval+generationWait)
	beforePrint := printJob()
	if !strings.Contains(beforePrint, "runs = 2") {
		t.Fatalf("expected two launchd runs: %s", beforePrint)
	}
	before, err := os.ReadFile(a.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, disableDirectly := range []bool{false, true} {
		if disableDirectly {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			out, err := exec.CommandContext(ctx, "/bin/launchctl", "disable", target).CombinedOutput()
			cancel()
			if err != nil {
				t.Fatalf("directly disable test job: %v\n%s", err, out)
			}
			if got := run("status"); !got.Loaded || !jobDisabled() {
				t.Fatalf("directly disabled job was not loaded and disabled: %+v", got)
			}
		}
		if got := run("install"); got.Changed || !got.Loaded || jobDisabled() {
			t.Fatalf("identical install changed registration or left it disabled: %+v", got)
		}
		after, err := os.ReadFile(a.PlistPath)
		if err != nil {
			t.Fatal(err)
		}
		afterInfo, err := os.Stat(a.PlistPath)
		if err != nil || !bytes.Equal(before, after) || !os.SameFile(info, afterInfo) || !info.ModTime().Equal(afterInfo.ModTime()) || !strings.Contains(printJob(), "runs = 2") {
			t.Fatalf("identical install rewrote or booted out the job: %v", err)
		}
	}
	if got := run("disable"); got.Loaded || !got.Installed {
		t.Fatalf("disable: %+v", got)
	}
	beforeDisabled := savedFiles(t, dir)
	t.Log("disabled job; observing one complete 60-second interval")
	time.Sleep(interval + 5*time.Second)
	if got := run("status"); got.Loaded || !got.Installed || !reflect.DeepEqual(beforeDisabled, savedFiles(t, dir)) {
		t.Fatalf("disabled job ran again: %+v", got)
	}
	for _, name := range []string{autolog.OutName, autolog.ErrName} {
		if err := os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte("x"), 2048), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A private server restart gives the enable cycle real recovery work and
	// produces a journal whose bytes uninstall must preserve.
	s.stop(t)
	s.start(t)
	if got := run("enable"); !got.Loaded || !got.Installed {
		t.Fatalf("enable: %+v", got)
	}
	third := waitGeneration(3, generationWait)
	if third.Envelope.InstanceID == first.Envelope.InstanceID {
		t.Fatal("enable did not observe the private server restart")
	}
	logDeadline := time.Now().Add(s.client.CallTimeout)
	for {
		out, outErr := os.ReadFile(filepath.Join(dir, autolog.OutName))
		diagnostic, diagnosticErr := os.ReadFile(filepath.Join(dir, autolog.ErrName))
		if outErr == nil && diagnosticErr == nil && len(out) <= 1024 && len(diagnostic) <= 1024 && bytes.Contains(out, []byte("log truncated\n")) && bytes.Contains(out, []byte(`"saved_generation":3`)) && bytes.Contains(diagnostic, []byte("log truncated\n")) {
			break
		}
		if time.Now().After(logDeadline) {
			t.Fatalf("automatic log bounds: %q %q %v %v", out, diagnostic, outErr, diagnosticErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Register a legacy plist with no WatchPaths, then install the same inputs.
	// Use an hour-long timer so the restart below cannot be timer-driven.
	agentInterval = time.Hour
	if got := run("install"); !got.Changed || !got.Loaded {
		t.Fatalf("long-interval install: %+v", got)
	}
	waitIdle()
	desired, err := os.ReadFile(a.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(desired, []byte("<key>WatchPaths</key>"))
	if start < 0 {
		t.Fatal("rendered plist lacks WatchPaths")
	}
	end := bytes.Index(desired[start:], []byte("</array>"))
	if end < 0 {
		t.Fatal("rendered WatchPaths is not an array")
	}
	end += start + len("</array>")
	legacy := append(append([]byte(nil), desired[:start]...), desired[end:]...)
	if got := run("disable"); got.Loaded {
		t.Fatalf("legacy setup disable: %+v", got)
	}
	if err := os.WriteFile(a.PlistPath, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if got := run("enable"); !got.Loaded {
		t.Fatalf("legacy setup enable: %+v", got)
	}
	waitIdle()
	if got := run("install"); !got.Changed || !got.Loaded {
		t.Fatalf("legacy WatchPaths upgrade: %+v", got)
	}
	upgraded, err := os.ReadFile(a.PlistPath)
	if err != nil || !bytes.Equal(upgraded, desired) {
		t.Fatalf("legacy plist was not replaced with the rendered plist: %v", err)
	}
	checkWatchPaths()
	beforeRuns := waitIdle()
	started := time.Now()
	s.stop(t)
	if _, err := os.Lstat(s.client.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private server did not remove its socket: %v", err)
	}
	for runs(printJob()) <= beforeRuns {
		if time.Since(started) > 15*time.Second {
			t.Fatal("socket removal did not start a cycle within 15 seconds")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("socket-removal cycle was observed too late: %s", elapsed)
	}
	t.Logf("socket removal started a waiting cycle after %s with StartInterval=%s", time.Since(started), agentInterval)
	s.start(t)
	fourth := waitGeneration(4, generationWait)
	if fourth.Envelope.InstanceID == third.Envelope.InstanceID {
		t.Fatal("WatchPaths cycle did not observe the private server restart")
	}
	logDeadline = time.Now().Add(s.client.CallTimeout)
	for {
		data, err := os.ReadFile(filepath.Join(dir, autolog.OutName))
		found := false
		for line := range bytes.SplitSeq(data, []byte("\n")) {
			var result AutoResult
			if json.Unmarshal(line, &result) == nil && result.State == "new_instance" && result.RestoreOutcome == "complete" && result.SavedGeneration == 4 && result.InstanceID == fourth.Envelope.InstanceID {
				found = true
			}
		}
		if err == nil && found {
			break
		}
		if time.Now().After(logDeadline) {
			t.Fatalf("WatchPaths cycle did not report completed recovery: %s %v", data, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := run("disable"); got.Loaded {
		t.Fatalf("final disable: %+v", got)
	}
	journals, err := filepath.Glob(filepath.Join(dir, "restore-*.json"))
	if err != nil || len(journals) == 0 {
		t.Fatalf("recovery did not publish a journal: %v %v", journals, err)
	}
	journal := journalFor(t, s.client, dir, *fourth.LastKnownGood)
	if journal.Status(*fourth.LastKnownGood).Outcome != "complete" {
		t.Fatal("enabled cycle did not complete journaled recovery")
	}
	otherPlist := filepath.Join(plistDir, "unrelated.plist")
	if err := os.WriteFile(otherPlist, []byte("untouched test sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	retained := savedFiles(t, dir)
	if got := run("uninstall"); got.Installed || got.Loaded {
		t.Fatalf("uninstall: %+v", got)
	}
	if jobDisabled() {
		t.Fatalf("uninstall left %s listed as disabled", label)
	}
	if _, err := os.Stat(a.PlistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned plist still exists: %v", err)
	}
	if got, err := os.ReadFile(otherPlist); err != nil || string(got) != "untouched test sentinel" || !reflect.DeepEqual(retained, savedFiles(t, dir)) {
		t.Fatalf("uninstall altered unrelated plist, generations, or journals: %v", err)
	}
	t.Logf("%s: scheduled generation 2, unchanged reinstall with direct-disable recovery, disabled interval, enabled recovery to generation 3, legacy WatchPaths upgrade, triggered recovery to generation 4, and byte-preserving uninstall verified (%d state files)", target, len(retained))
}
