package rex

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This executable never delegates to Rex. It logs each invocation before
// delaying so a killed or failed mutation cannot hide a duplicate attempt.
const slowRexScript = `
if [ "$1" != --server ] || [ "$2" != unix:///private/tmp/rex-client-test-unused ] || [ "$3" != --autostart=false ] || [ "$4" != --timeout ] || [ "$5" != "$EXPECTED_TIMEOUT" ]; then
  printf 'unexpected Rex arguments\n' >&2; exit 2
fi
method=$8
if [ "$6" = whoami ]; then method=whoami; fi
n=0
if [ -f "$COUNTS/$method" ]; then read -r n < "$COUNTS/$method"; fi
n=$((n+1))
printf '%s\n' "$n" > "$COUNTS/$method"
case "$MODE" in
  fail) printf 'fixture failure\n' >&2; exit 1 ;;
  timeout) exec /bin/sleep 60 ;;
  slow)
    if [ "$n" = 1 ]; then printf 'transient fixture failure\n' >&2; exit 1; fi
    if [ "$n" = 2 ]; then /bin/sleep 6; fi ;;
  restore)
    case "$method" in
      session.create) exec /bin/sleep 60 ;;
      session.list) printf '%s\n' '{"sessions":[]}'; exit 0 ;;
    esac ;;
esac
case "$method" in
  whoami) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  session.list) printf '%s\n' '{"sessions":[{"session_id":"session"}]}' ;;
  session.view) printf '%s\n' '{"session_id":"session","revision":1,"label":"saved","active_window_id":"window","windows":[{"window_id":"window","label":"shell","focused_block_id":"block","layers":[{"layer_id":"layer","kind":"tiled","layout":{"block_id":"block"},"blocks":[{"block_id":"block","label":"terminal","flavor":"com.superlogical.terminal.shell"}]}]}]}' ;;
  session.list_blocks) printf '%s\n' '{"blocks":[{"block_id":"block","label":"terminal","flavor":"com.superlogical.terminal.shell","placement":"placed","window_id":"window"}]}' ;;
  com.superlogical.terminal.pwd) printf '%s\n' '{"pwd":null}' ;;
  com.superlogical.terminal.process) printf '%s\n' '{"child":{"cwd":"/private/tmp"}}' ;;
  *) printf 'unexpected method: %s\n' "$method" >&2; exit 1 ;;
esac
`

func TestSlowRexCalls(t *testing.T) {
	newClient := func(t *testing.T, mode string, timeout time.Duration) (Client, func(string, int)) {
		t.Helper()
		counts := t.TempDir()
		effective := timeout
		if effective == 0 {
			effective = DefaultCallTimeout
		}
		c := scriptClient(t, slowRexScript, "COUNTS="+counts, "MODE="+mode, "EXPECTED_TIMEOUT="+effective.String())
		c.CallTimeout = timeout
		return c, func(method string, want int) {
			t.Helper()
			data, err := os.ReadFile(filepath.Join(counts, method))
			if err != nil || strings.TrimSpace(string(data)) != strconv.Itoa(want) {
				t.Fatalf("%s invoked %q times, want %d: %v", method, data, want, err)
			}
		}
	}

	seed := func(t *testing.T, dir string) {
		t.Helper()
		c, _ := newClient(t, "ready", 0)
		if _, err := c.Save(t.Context(), dir, false); err != nil {
			t.Fatal(err)
		}
	}
	// Every read-only method fails once, then takes six seconds to reply. The
	// default client and an explicitly configured client must both complete.
	for _, phase := range []string{"save", "status", "auto"} {
		t.Run(phase, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "saved")
			timeout := time.Duration(0)
			if phase != "save" {
				seed(t, dir)
				timeout = 15 * time.Second
			}
			c, count := newClient(t, "slow", timeout)
			ctx, cancel := context.WithTimeout(t.Context(), DefaultAutoTimeout)
			defer cancel()
			switch phase {
			case "save":
				saved, err := c.Save(ctx, dir, false)
				if err != nil || saved.Generation != 1 || len(saved.Snapshot.Sessions) != 1 {
					t.Fatalf("slow save: %+v %v", saved, err)
				}
			case "status":
				status, err := c.Status(ctx, dir)
				if err != nil || status.Generation != 1 || status.Blocks != 1 {
					t.Fatalf("slow status: %+v %v", status, err)
				}
				count("whoami", 2)
				count("server.version", 2)
				return
			case "auto":
				out, err := c.AutoCycle(ctx, dir, DefaultAutoWait, DefaultAutoTimeout)
				if err != nil || out.State != "same_instance" || out.SavedGeneration != 0 || out.Reason != "unchanged" {
					t.Fatalf("slow auto: %+v %v", out, err)
				}
			}
			whoamiCalls := 3
			if phase == "auto" {
				whoamiCalls++
			}
			count("whoami", whoamiCalls)
			count("server.version", 3)
			count("session.list", 3)
			count("session.view", 4)
			count("session.list_blocks", 2)
			count("com.superlogical.terminal.pwd", 2)
			count("com.superlogical.terminal.process", 2)
		})
	}

	t.Run("persistent-read-failure", func(t *testing.T) {
		c, count := newClient(t, "fail", time.Second)
		err := c.Call(t.Context(), "session.list", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "after 3 attempts") || !strings.Contains(err.Error(), "fixture failure") {
			t.Fatalf("persistent read error: %v", err)
		}
		count("session.list", 3)
	})
	t.Run("read-timeout", func(t *testing.T) {
		c, count := newClient(t, "timeout", time.Second)
		budget := 3*c.CallTimeout + 2*readRetryDelay
		ctx, cancel := context.WithTimeout(t.Context(), budget+DefaultCallTimeout)
		defer cancel()
		started := time.Now()
		err := c.Call(ctx, "session.view", nil, nil)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "after 3 attempts") || time.Since(started) > budget+5*time.Second {
			t.Fatalf("read timeout after %s: %v", time.Since(started), err)
		}
		count("session.view", 3)
	})
	t.Run("mutations", func(t *testing.T) {
		c, count := newClient(t, "timeout", time.Second)
		for _, method := range []string{"session.create", "session.focus_block", "session.focus_window", "session.zoom_block", "session.set_label", "session.destroy", "session.future_method"} {
			if err := c.Call(t.Context(), method, nil, nil); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s: %v", method, err)
			}
			count(method, 1)
		}
	})
	t.Run("uncertain-create", func(t *testing.T) {
		c, count := newClient(t, "restore", time.Second)
		dir := filepath.Join(t.TempDir(), "saved")
		seed(t, dir)
		out, err := c.RestoreSaved(t.Context(), dir, 0)
		if !errors.Is(err, context.DeadlineExceeded) || out.Outcome != "uncertain" || !strings.Contains(err.Error(), "session.create outcome is uncertain") {
			t.Fatalf("delayed create: %+v %v", out, err)
		}
		before := savedFiles(t, dir)
		if _, err := c.RestoreSaved(t.Context(), dir, 0); err == nil || !strings.Contains(err.Error(), "zero live matches") {
			t.Fatalf("read-back of uncertain create: %v", err)
		}
		count("session.create", 1)
		if !maps.Equal(before, savedFiles(t, dir)) {
			t.Fatal("uncertain read-back changed state")
		}
	})
}
