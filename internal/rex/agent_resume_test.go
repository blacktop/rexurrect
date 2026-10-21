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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// commandReceipt is the host and command form that `receipt set` registers.
func commandReceipt(n int, host, command string) receipt.Receipt {
	r := agentReceipt(n, "")
	r.Host, r.Command = host, command
	return r
}

// agentReceipt is the legacy harness form that older stores and snapshots hold.
func agentReceipt(n int, harness string) receipt.Receipt {
	return receipt.Receipt{SessionID: "session:01234567-89ab-4cde-8fab-0123456789ab", BlockID: fmt.Sprintf("block:01234567-89ab-4cde-8fab-%012x", n), Harness: harness, Conversation: fmt.Sprintf("01234567-89ab-4cde-8fab-%012x", n), Model: "model-1.2", Effort: "high"}
}

func buildReceiptCLI(t *testing.T, dir string) string {
	t.Helper()
	binary := filepath.Join(dir, "rexurrect")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../cmd/rexurrect")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build receipt CLI: %v\n%s", err, out)
	}
	return binary
}

func TestReceiptHooksWaitForStateLock(t *testing.T) {
	if DefaultLockTimeout < 10*time.Second {
		t.Fatal("receipt lock wait is too short")
	}
	r := commandReceipt(1, "codex", "codex")
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[{"block_id":"`+r.BlockID+`","flavor":"com.superlogical.terminal.shell"}]}' ;;
  *) exit 1 ;;
esac
`)
	root := t.TempDir()
	binary, dir := buildReceiptCLI(t, root), filepath.Join(root, "saved")
	for _, action := range []string{"set", "clear"} {
		t.Run(action, func(t *testing.T) {
			release := holdAutoLock(t, dir)
			done := make(chan struct{})
			go func() {
				time.Sleep(time.Second)
				release()
				close(done)
			}()
			args := []string{"receipt", action, "--server", "unix://" + c.Socket, "--state-dir", dir, "--rex", c.Binary, "--session", r.SessionID, "--block", r.BlockID, "--conversation", r.Conversation}
			if action == "set" {
				args = append(args, "--host", r.Host, "--command", r.Command)
			}
			started := time.Now()
			out, err := exec.CommandContext(t.Context(), binary, args...).CombinedOutput()
			elapsed := time.Since(started)
			<-done
			if err != nil || elapsed < 900*time.Millisecond {
				t.Fatalf("contended receipt %s after %s: %v\n%s", action, elapsed, err, out)
			}
		})
	}
	if got, err := c.ListReceipts(t.Context(), dir); err != nil || len(got) != 0 {
		t.Fatalf("clear did not persist: %+v %v", got, err)
	}
	for _, action := range []string{"set", "clear"} {
		t.Run(action+"-timeout", func(t *testing.T) {
			release := holdAutoLock(t, dir)
			defer release()
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			var err error
			if action == "set" {
				err = c.SetReceipt(ctx, dir, r)
			} else {
				err = c.ClearReceipt(ctx, dir, r.Key, receipt.ClearGuard{Conversation: r.Conversation})
			}
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, state.ErrLocked) || !strings.Contains(err.Error(), "waiting for state lock") {
				t.Fatalf("receipt %s timeout: %v", action, err)
			}
		})
	}
}

func TestReceiptLockTimeoutFlagBoundsWait(t *testing.T) {
	r := commandReceipt(1, "codex", "codex")
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[{"block_id":"`+r.BlockID+`","flavor":"com.superlogical.terminal.shell"}]}' ;;
  *) exit 1 ;;
esac
`)
	root := t.TempDir()
	binary, dir := buildReceiptCLI(t, root), filepath.Join(root, "saved")
	if err := c.SetReceipt(t.Context(), dir, r); err != nil {
		t.Fatal(err)
	}
	release := holdAutoLock(t, dir)
	for _, action := range []string{"set", "clear"} {
		t.Run(action, func(t *testing.T) {
			args := []string{
				"receipt", action, "--server", "unix://" + c.Socket, "--state-dir", dir,
				"--rex", c.Binary, "--lock-timeout", "300ms",
				"--session", r.SessionID, "--block", r.BlockID,
				"--conversation", r.Conversation, "--command", r.Command,
			}
			if action == "set" {
				args = append(args, "--host", r.Host)
			}
			started := time.Now()
			out, err := exec.CommandContext(t.Context(), binary, args...).CombinedOutput()
			elapsed := time.Since(started)
			var exit *exec.ExitError
			bounded := elapsed >= 300*time.Millisecond && elapsed <= 5*time.Second
			reported := strings.Contains(string(out), "waiting for state lock")
			if !errors.As(err, &exit) || !bounded || !reported {
				t.Fatalf("held lock: %s err=%v elapsed=%s\n%s", action, err, elapsed, out)
			}
		})
	}
	release()
	if got, err := c.ListReceipts(t.Context(), dir); err != nil || len(got) != 1 || got[0] != r {
		t.Fatalf("bounded failure changed the receipt: %+v %v", got, err)
	}
	c.LockTimeout = MaxLockTimeout + time.Second
	if _, err := c.ListReceipts(t.Context(), dir); err == nil {
		t.Fatal("client accepted lock timeout above the cap")
	}
}

func TestAgentInitialInputOptions(t *testing.T) {
	for _, harness := range []string{"claude", "claude-ddb", "codex", "codex-team", "claude-team", "unknown"} {
		r := agentReceipt(1, harness)
		var leaves []snapshot.SourceID
		n := layout(snapshot.Node{Block: &snapshot.Block{ID: snapshot.SourceID(r.BlockID), CWD: "/", AgentResume: &r}}, &leaves)
		data, err := json.Marshal(n.Block.Options)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"cwd": "/"}
		if input := r.InitialInput(); input != "" {
			want["initial_input"] = input
		}
		if !reflect.DeepEqual(fields, want) {
			t.Fatalf("%s: %s", harness, data)
		}
	}
}

func TestReceiptRegistrationAndPruning(t *testing.T) {
	s := newIsolatedServer(t)
	var created createdSession
	mustCall(t, s.client, "session.create", map[string]any{"label": "receipts", "initial_windows": []createWindow{{Label: "first", Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}}}}, &created)
	if len(created.Windows) != 1 || len(created.Windows[0].Blocks) != 1 {
		t.Fatalf("create: %+v", created)
	}
	r := agentReceipt(1, "codex")
	r.SessionID, r.BlockID = string(created.ID), string(created.Windows[0].Blocks[0])
	dir := filepath.Join(s.root, "saved")
	missing := r
	missing.BlockID = agentReceipt(2, "codex").BlockID
	if err := s.client.SetReceipt(t.Context(), dir, missing); err == nil {
		t.Fatal("registered missing block")
	}
	if err := s.client.SetReceipt(t.Context(), dir, r); err != nil {
		t.Fatal(err)
	}
	e, err := s.client.Save(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot.Sessions[0].Windows[0].Layout.Block.AgentResume; got == nil || *got != r {
		t.Fatalf("attached receipt: %+v", got)
	}
	mustCall(t, s.client, "session.destroy", map[string]any{"session_id": created.ID}, nil)
	before := savedFiles(t, dir)
	if _, err := s.client.Save(t.Context(), dir, false); err == nil {
		t.Fatal("empty guard did not reject")
	}
	if !reflect.DeepEqual(before, savedFiles(t, dir)) {
		t.Fatal("rejected save pruned receipts")
	}
	e, err = s.client.Save(t.Context(), dir, true)
	if err != nil || !slices.Equal(e.Report.PrunedReceipts, []snapshot.SourceID{snapshot.SourceID(r.BlockID)}) {
		t.Fatalf("prune: %+v %v", e.Report, err)
	}
	if got, err := s.client.ListReceipts(t.Context(), dir); err != nil || len(got) != 0 {
		t.Fatalf("retained stale receipt: %+v %v", got, err)
	}
}

func TestAutoFinishesPublishedReceiptPruning(t *testing.T) {
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.list) printf '%s\n' '{"sessions":[]}' ;;
  *) exit 1 ;;
esac
`)
	dir := filepath.Join(t.TempDir(), "saved")
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := agentReceipt(1, "codex")
	if err := store.SetReceipt("host", r); err != nil {
		t.Fatal(err)
	}
	// Reproduce the durable intermediate state: publication succeeded, but
	// the following receipt-file update did not run.
	e, err := c.stableCapture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachReceipts(store, &e); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(e, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before := savedFiles(t, dir)
	out, err := c.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
	if err != nil || out.Reason != "unchanged" || out.SavedGeneration != 0 || !slices.Equal(out.PrunedReceipts, []snapshot.SourceID{snapshot.SourceID(r.BlockID)}) {
		t.Fatalf("finish pruning: %+v %v", out, err)
	}
	if got, err := c.ListReceipts(t.Context(), dir); err != nil || len(got) != 0 {
		t.Fatalf("stale receipt remains: %+v %v", got, err)
	}
	after := savedFiles(t, dir)
	delete(before, "receipts.json")
	delete(after, "receipts.json")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("finishing published pruning changed generation or pointer bytes")
	}
}

func TestReceiptRebindingPreservesLiveSources(t *testing.T) {
	r := agentReceipt(1, "codex")
	mapped := agentReceipt(2, "codex")
	mapped.SessionID = "session:01234567-89ab-4cde-8fab-000000000002"
	c := scriptClient(t, `printf '%s\n' '{"sessions":[{"session_id":"`+r.SessionID+`"}]}'`)
	store, err := state.Open(filepath.Join(t.TempDir(), "saved"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := store.SetReceipt("host", r); err != nil {
		t.Fatal(err)
	}
	session := snapshot.Session{ID: snapshot.SourceID(r.SessionID), Windows: []snapshot.Window{{Layout: snapshot.Node{Block: &snapshot.Block{ID: snapshot.SourceID(r.BlockID), AgentResume: &r}}}}}
	ids := snapshot.IDMap{session.ID: snapshot.LiveID(mapped.SessionID), snapshot.SourceID(r.BlockID): snapshot.LiveID(mapped.BlockID)}
	if err := c.rebindReceipts(t.Context(), store, "host", session, ids); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadReceipts("host")
	want := r
	want.Key = mapped.Key
	if err != nil || len(got) != 2 || !slices.Contains(got, r) || !slices.Contains(got, want) {
		t.Fatalf("lost live source receipt: %+v %v", got, err)
	}
}

func TestAgentResumeRoundTrip(t *testing.T) {
	requireLoginShell(t)
	var marker, exited string
	s := newIsolatedServerWithSetup(t, func(s *isolatedServer) {
		marker = filepath.Join(s.root, "agent-argv")
		exited = filepath.Join(s.root, "agent-exited")
		binary := buildReceiptCLI(t, s.root)
		for _, path := range []string{marker, exited} {
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		// Both paths are inside the private HOME. XDG_CONFIG_HOME is explicit
		// in this server's environment, and .config also covers login defaults.
		for _, base := range []string{"config", ".config"} {
			dir := filepath.Join(s.root, "home", base, "fish", "functions")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			// Custom command names register through the real receipt CLI with an
			// explicit host. Legacy-only names never get a live function that
			// registers; a typed command for them would be a test failure.
			for name, host := range map[string]string{"claude-work": "claude", "codex-work": "codex", "claude-team": "", "future-agent": ""} {
				body := fmt.Sprintf(`function %[1]s
    set -l rr_id $argv[2]
    set -l rr_effort (string replace 'model_reasoning_effort=' '' -- $argv[6])
    '%[2]s' receipt set --server 'unix://%[3]s' --state-dir '%[4]s' --rex '%[5]s' --session "$REX_SESSION" --block "$REX_BLOCK" --host %[8]s --command %[1]s --conversation "$rr_id" --model "$argv[4]" --effort "$rr_effort"
    or return
    printf '%%s\t%%s\t%%s\t%%s\n' "$REX_SESSION" "$REX_BLOCK" (pwd -P) (string join \t -- %[1]s $argv) >> '%[6]s'
    read -l rr_wait; or return
    '%[2]s' receipt clear --server 'unix://%[3]s' --state-dir '%[4]s' --rex '%[5]s' --session "$REX_SESSION" --block "$REX_BLOCK" --command %[1]s --conversation "$rr_id"
    or return
    printf '%%s\n' "$REX_BLOCK" >> '%[7]s'
end
`, name, binary, s.client.Socket, filepath.Join(s.root, "saved"), s.client.Binary, marker, exited, host)
				if host == "" {
					body = fmt.Sprintf("function %[1]s\n    printf 'unexpected %[1]s\\n' >> '%[2]s'\nend\n", name, marker)
				}
				if err := os.WriteFile(filepath.Join(dir, name+".fish"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	readLines := func(path string) []string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			return nil
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		slices.Sort(lines)
		return lines
	}
	waitLines := func(path string, want []string) {
		t.Helper()
		want = slices.Clone(want)
		slices.Sort(want)
		ctx, cancel := context.WithTimeout(t.Context(), DefaultAutoWait)
		defer cancel()
		for {
			got := readLines(path)
			if slices.Equal(got, want) {
				return
			}
			if len(got) >= len(want) {
				t.Fatalf("agent argv: %q; want %q", got, want)
			}
			select {
			case <-ctx.Done():
				t.Fatalf("agent argv: %q; want %q: %v", got, want, ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	readMarker := func() []string { return readLines(marker) }
	waitMarker := func(want []string) { waitLines(marker, want) }
	cwd := filepath.Join(s.root, "agent work")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	// Two live command-form registrations, one legacy inert receipt
	// (claude-team never resumed under store format 1 and must stay that
	// way), and one legacy receipt crafted to an unknown future name.
	harnesses := []string{"claude-work", "codex-work", "claude-team", "future-agent"}
	sourceArgs := [][]string{
		{"claude-work", "--resume", agentReceipt(1, "").Conversation, "--model", "model-1.2", "--effort", "high"},
		{"codex-work", "resume", agentReceipt(2, "").Conversation, "-m", "model-1.2", "-c", "model_reasoning_effort=high"},
		nil,
		nil,
	}
	windows := make([]createWindow, 0, len(harnesses))
	for i, h := range harnesses {
		var input string
		if len(sourceArgs[i]) != 0 {
			input = strings.Join(sourceArgs[i], " ") + "\n"
		}
		windows = append(windows, createWindow{Label: h, Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Label: h, Options: shellOptions{CWD: cwd, InitialInput: input}}}})
	}
	var created createdSession
	mustCall(t, s.client, "session.create", map[string]any{"label": "agents", "initial_windows": windows}, &created)
	if len(created.Windows) != len(harnesses) {
		t.Fatalf("create: %+v", created)
	}
	dir := filepath.Join(s.root, "saved")
	var initial []string
	for i, h := range harnesses {
		if len(created.Windows[i].Blocks) != 1 {
			t.Fatalf("window: %+v", created.Windows[i])
		}
		block := created.Windows[i].Blocks[0]
		waitDefaultTerminal(t, s.client, created.ID, block, cwd)
		if len(sourceArgs[i]) == 0 {
			// Legacy receipts are registered through the Go store API, as an
			// older binary would have written them; "codex" is crafted into an
			// unknown future name below.
			legacy := "claude-team"
			if h == "future-agent" {
				legacy = "codex"
			}
			r := agentReceipt(i+1, legacy)
			r.SessionID, r.BlockID = string(created.ID), string(block)
			if err := s.client.SetReceipt(t.Context(), dir, r); err != nil {
				t.Fatal(err)
			}
			continue
		}
		initial = append(initial, strings.Join(append([]string{string(created.ID), string(block), cwd}, sourceArgs[i]...), "\t"))
	}
	waitMarker(initial)
	saved, err := s.client.Save(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range saved.Snapshot.Sessions[0].Blocks() {
		if b.AgentResume == nil {
			t.Fatalf("missing receipt: %s", b.ID)
		}
	}
	path := filepath.Join(dir, fmt.Sprintf("generation-%020d.json", saved.Generation))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crafted := bytes.Replace(data, []byte(`"harness":"codex"`), []byte(`"harness":"future-agent"`), 1)
	if bytes.Equal(data, crafted) {
		t.Fatal("future harness fixture did not change")
	}
	if err := os.WriteFile(path, crafted, 0600); err != nil {
		t.Fatal(err)
	}
	saved = loadSaved(t, dir).Envelope
	firstPID := s.cmd.Process.Pid
	s.stop(t)
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.start(t)
	if s.cmd.Process.Pid == firstPID || len(listSessions(t, s.client)) != 0 {
		t.Fatal("restart was not empty")
	}
	// Exercise the actual automatic cycle, which calls restoreSaved and then
	// captures a new generation under the same lock.
	auto, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
	if err != nil || auto.State != "new_instance" || auto.RestoreOutcome != "complete" {
		t.Fatalf("auto: %+v %v", auto, err)
	}
	// Each marker follows a successful receipt registration. Wait until both
	// hooks have released the lock before the test's nonblocking state reads.
	ready, cancel := context.WithTimeout(t.Context(), DefaultAutoWait)
	defer cancel()
	for len(readMarker()) < 2 {
		select {
		case <-ready.Done():
			t.Fatalf("replayed hooks did not register: %q: %v", readMarker(), ready.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	j := journalFor(t, s.client, dir, saved)
	result := j.Status(saved)
	team := snapshot.SourceID(created.Windows[2].Blocks[0])
	future := snapshot.SourceID(created.Windows[3].Blocks[0])
	if len(result.NotResumed) != 2 || result.NotResumed[0].BlockID != team || result.NotResumed[0].Harness != "claude-team" || result.NotResumed[1].BlockID != future || result.NotResumed[1].Harness != "future-agent" || !reflect.DeepEqual(auto.NotResumed, result.NotResumed) {
		t.Fatalf("not resumed: %+v / %+v", result, auto)
	}
	if len(j.Records) == 0 || !reflect.DeepEqual(j.Records[0].NotResumed, result.NotResumed) {
		t.Fatalf("journal lost skip: %+v", j)
	}
	var want []string
	for i, h := range harnesses[:2] {
		r := agentReceipt(i+1, "")
		args := []string{h, "--resume", r.Conversation, "--model", r.Model, "--effort", r.Effort}
		if h == "codex-work" {
			args = []string{h, "resume", r.Conversation, "-m", r.Model, "-c", "model_reasoning_effort=" + r.Effort}
		}
		want = append(want, strings.Join(append([]string{string(result.IDMap[snapshot.SourceID(created.ID)]), string(result.IDMap[snapshot.SourceID(created.Windows[i].Blocks[0])]), cwd}, args...), "\t"))
	}
	waitMarker(want)
	compareRestored(t, s.client, saved, result.IDMap)
	for _, b := range saved.Snapshot.Sessions[0].Blocks() {
		waitDefaultTerminal(t, s.client, result.IDMap[saved.Snapshot.Sessions[0].ID], result.IDMap[b.ID], b.CWD)
	}
	current := loadSaved(t, dir).Envelope
	if current.Generation != saved.Generation+1 {
		t.Fatalf("new generation: %d", current.Generation)
	}
	for _, b := range current.Snapshot.Sessions[0].Blocks() {
		if snapshot.LiveID(b.ID) == result.IDMap[future] {
			if b.AgentResume != nil {
				t.Fatalf("unknown harness was registered: %+v", b)
			}
			continue
		}
		if b.AgentResume == nil || b.AgentResume.BlockID != string(b.ID) {
			t.Fatalf("receipt not rebound: %+v", b)
		}
	}
	before, files := readMarker(), savedFiles(t, dir)
	again, err := s.client.RestoreSaved(t.Context(), dir, saved.Generation)
	if err != nil || !reflect.DeepEqual(again, result) || !reflect.DeepEqual(before, readMarker()) || !reflect.DeepEqual(files, savedFiles(t, dir)) {
		t.Fatalf("second restore changed state: %+v %v", again, err)
	}
	var finished []string
	for _, w := range created.Windows[:2] {
		block := result.IDMap[snapshot.SourceID(w.Blocks[0])]
		mustCall(t, s.client, "com.superlogical.terminal.write", map[string]any{"session_id": result.IDMap[snapshot.SourceID(created.ID)], "block_id": block, "args": map[string]any{"data": "\n"}}, nil)
		finished = append(finished, string(block))
	}
	waitLines(exited, finished)
	entries, err := s.client.ListReceipts(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range entries {
		if slices.Contains(finished, r.BlockID) {
			t.Fatalf("exited replay retained receipt: %+v", r)
		}
	}
	beforeFiles := savedFiles(t, dir)
	if again, err := s.client.RestoreSaved(t.Context(), dir, saved.Generation); err != nil || !reflect.DeepEqual(again, result) || !reflect.DeepEqual(beforeFiles, savedFiles(t, dir)) || !reflect.DeepEqual(before, readMarker()) {
		t.Fatalf("completed restore re-registered an exited agent: %+v %v", again, err)
	}
	t.Logf("resumed and cleared two custom-command conversations; legacy claude-team and future-agent received no input; compared %d ID mappings", len(result.IDMap))
}

func TestReceiptRejectsBeforeRex(t *testing.T) {
	log := filepath.Join(t.TempDir(), "called")
	c := scriptClient(t, `printf called > "$CALLS"; exit 1`, "CALLS="+log)
	r := agentReceipt(1, "codex")
	r.Conversation += ";touch /tmp/no"
	if err := c.SetReceipt(t.Context(), filepath.Join(t.TempDir(), "saved"), r); err == nil {
		t.Fatal("accepted injection")
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid receipt invoked Rex: %v", err)
	}
}
