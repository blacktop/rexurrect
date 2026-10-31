package rex

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The counter is published by rename, so a read never sees it truncated
// between a call's truncate and write.
const awaitScript = `n=$(cat "$COUNTER" 2>/dev/null || echo 0); n=$((n+1)); printf '%s' "$n" > "$COUNTER.tmp" && mv -f "$COUNTER.tmp" "$COUNTER"
if [ "$n" -lt "$READY_AT" ]; then echo "not yet" >&2; exit 1; fi
printf '{"host_id":"host","instance_id":"inst","rex_version":"v","pid":%s}\n' "$SERVER_PID"
`

func awaitClient(t *testing.T, readyAt, pid int) (Client, string) {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "calls")
	c := scriptClient(t, awaitScript, "COUNTER="+counter, "READY_AT="+strconv.Itoa(readyAt), "SERVER_PID="+strconv.Itoa(pid))
	c.CallTimeout = DefaultCallTimeout
	return c, counter
}

func calls(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAwaitServer(t *testing.T) {
	always := func() bool { return true }
	c, counter := awaitClient(t, 4, 321)
	got, err := c.AwaitServer(t.Context(), 10*time.Second, always)
	if err != nil || got != 321 || calls(t, counter) != 4 {
		t.Fatalf("ready after retries: %d %v, %d calls", got, err, calls(t, counter))
	}
	c, counter = awaitClient(t, 1, 321)
	if got, err := c.AwaitServer(t.Context(), time.Second, func() bool { return false }); err != nil || got != 0 || calls(t, counter) != 0 {
		t.Fatalf("gone parent still polled: %d %v, %d calls", got, err, calls(t, counter))
	}
	c, counter = awaitClient(t, 1000, 321)
	start := time.Now()
	if got, err := c.AwaitServer(t.Context(), 700*time.Millisecond, always); err != nil || got != 0 || calls(t, counter) == 0 || time.Since(start) < 700*time.Millisecond {
		t.Fatalf("wait deadline: %d %v, %d calls after %s", got, err, calls(t, counter), time.Since(start))
	}
	remaining := 2
	c, counter = awaitClient(t, 1000, 321)
	start = time.Now()
	if got, err := c.AwaitServer(t.Context(), 10*time.Second, func() bool { remaining--; return remaining >= 0 }); err != nil || got != 0 || calls(t, counter) == 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("parent exit mid-wait: %d %v, %d calls after %s", got, err, calls(t, counter), time.Since(start))
	}
	c, _ = awaitClient(t, 1, 0)
	if _, err := c.AwaitServer(t.Context(), time.Second, always); err == nil || !strings.Contains(err.Error(), "no PID") {
		t.Fatalf("missing pid: %v", err)
	}
	if _, err := c.AwaitServer(t.Context(), 0, always); err == nil {
		t.Fatal("accepted a zero wait")
	}
	c, counter = awaitClient(t, 1, 321)
	if got, err := c.AwaitServer(t.Context(), 10*time.Second, func() bool { return calls(t, counter) == 0 }); err != nil || got != 0 {
		t.Fatalf("parent exited during successful reply: %d %v", got, err)
	}
}
