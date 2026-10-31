package rex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// The shell hooks take saved input only at an empty prompt, inside the
// shell: fish runs a receipt and types a foreground command, zsh types both,
// pending input is left alone, and every block ends with one record that
// matches what the pane shows. Panes run the real shells with the hook
// loaded from the private home's shell config.
func TestDeliveryHooksPrivateServer(t *testing.T) {
	for _, shell := range []string{"fish", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s not installed", shell)
			}
			dir := filepath.Join(t.TempDir(), "saved")
			s := newIsolatedServerWithSetup(t, func(s *isolatedServer) { installHook(t, s, shell, dir) })
			leaf := func(label string) createWindow {
				return createWindow{Label: label, Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Label: label, Options: shellOptions{Shell: path, Login: new(bool), CWD: s.root}}}}
			}
			var created createdSession
			mustCall(t, s.client, "session.create", map[string]any{"label": "hooks", "initial_windows": []createWindow{leaf("receipt"), leaf("typed"), leaf("pending")}}, &created)
			blocks := make([]snapshot.LiveID, 3)
			for i := range blocks {
				blocks[i] = created.Windows[i].Blocks[0]
			}
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			hooks := awaitHooks(t, store, blocks)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			for i, hook := range hooks {
				if hook.Shell != shell {
					t.Fatalf("pane %d announced %+v", i, hook)
				}
			}
			resume := receipt.Receipt{Key: receipt.Key{SessionID: string(created.ID), BlockID: string(blocks[0])}, Host: "claude", Command: "claude", Conversation: "01a18c00-0000-7000-8000-0000000000cc"}
			if err := s.client.SetReceipt(t.Context(), dir, resume); err != nil {
				t.Fatal(err)
			}
			writeForeground(t, dir, snapshot.SourceID(blocks[1]), "echo typed-not-run", 0)
			writeForeground(t, dir, snapshot.SourceID(blocks[2]), "echo must-not-appear", 0)
			mustCall(t, s.client, "com.superlogical.terminal.write", map[string]any{"session_id": created.ID, "block_id": blocks[2], "args": map[string]any{"data": "echo pending"}}, nil)
			waitPaneText(t, s.client, created.ID, blocks[2], "echo pending")
			e, err := s.client.Save(t.Context(), dir, false)
			if err != nil {
				t.Fatal(err)
			}
			target, err := s.client.identity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			latest := make(map[snapshot.SourceID]state.JournalRecord)
			for _, session := range e.Snapshot.Sessions {
				latest[session.ID] = state.JournalRecord{SourceID: session.ID, Outcome: "native", IDMap: identity(t, snapshot.Single(session))}
			}
			store, err = state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			deliveries, changed, err := s.client.deliverNative(t.Context(), store, e, latest, target)
			if err := errors.Join(err, store.Close()); err != nil || !changed || len(deliveries) != 3 {
				t.Fatalf("deliveries: %+v changed=%v %v", deliveries, changed, err)
			}
			byBlock := make(map[snapshot.SourceID]state.Delivery)
			for _, d := range deliveries {
				byBlock[d.BlockID] = d
				if d.Shell != shell {
					t.Errorf("delivery %+v did not see %s", d, shell)
				}
			}
			wantReceipt := state.DeliveryTypedOnly
			if shell == "fish" {
				wantReceipt = state.DeliveryResumed
			}
			if d := byBlock[snapshot.SourceID(blocks[0])]; d.Outcome != wantReceipt || d.Kind != state.DeliveryReceipt {
				t.Errorf("receipt pane: %+v, want %s", d, wantReceipt)
			}
			if d := byBlock[snapshot.SourceID(blocks[1])]; d.Outcome != state.DeliveryTypedOnly || d.Kind != state.DeliveryForeground {
				t.Errorf("typed pane: %+v", d)
			}
			if d := byBlock[snapshot.SourceID(blocks[2])]; d.Outcome != state.DeliverySkipped || !strings.Contains(d.Reason, "already had input") {
				t.Errorf("pending pane: %+v", d)
			}
			// The screens agree with the records: the receipt pane ran its
			// command (fish) or holds it at the prompt (zsh), the typed pane
			// holds its command unexecuted, and the pending pane still holds
			// what the user typed and nothing else.
			resumeLine := strings.TrimSuffix(resume.InitialInput(), "\n")
			receiptScreen := waitPaneText(t, s.client, created.ID, blocks[0], resumeLine)
			if ran := strings.Contains(receiptScreen, "claude: command not found") || strings.Contains(receiptScreen, "Unknown command: claude"); ran != (shell == "fish") {
				t.Errorf("receipt pane ran=%v on %s:\n%s", ran, shell, receiptScreen)
			}
			typedScreen := waitPaneText(t, s.client, created.ID, blocks[1], "echo typed-not-run")
			if strings.Contains(typedScreen, "\ntyped-not-run") {
				t.Errorf("typed pane ran its command:\n%s", typedScreen)
			}
			pendingScreen := paneText(t, s.client, created.ID, blocks[2])
			if !strings.Contains(pendingScreen, "echo pending") || strings.Contains(pendingScreen, "must-not-appear") {
				t.Errorf("pending pane changed:\n%s", pendingScreen)
			}
			// Nothing is asked twice, and the mailboxes hold no request.
			store, err = state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			again, changed, err := s.client.deliverNative(t.Context(), store, e, latest, target)
			if err != nil || changed || len(again) != 3 {
				t.Fatalf("second pass: %+v changed=%v %v", again, changed, err)
			}
			for _, block := range blocks {
				if revoked, err := store.RevokeDeliveryRequest(snapshot.SourceID(block)); err != nil || revoked {
					t.Errorf("block %s still had a request: revoked=%v %v", block, revoked, err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// installHook points the private home's shell config at rexurrect's hook
// for shell, with the state directory the test saves into.
func installHook(t *testing.T, s *isolatedServer, shell, dir string) {
	t.Helper()
	hook := hookScript(t, shell)
	home := filepath.Join(s.root, "home")
	var config, content string
	switch shell {
	case "fish":
		config = filepath.Join(home, "config", "fish", "config.fish")
		content = "set -gx REXURRECT_STATE_DIR " + dir + "\nsource " + hook + "\n"
	case "zsh":
		config = filepath.Join(home, ".zshrc")
		content = "export REXURRECT_STATE_DIR=" + dir + "\nsource " + hook + "\n"
	default:
		t.Fatalf("no hook config for %s", shell)
	}
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// awaitHooks waits until every pane's hook has announced itself.
func awaitHooks(t *testing.T, store *state.Store, blocks []snapshot.LiveID) []state.DeliveryHook {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		hooks := make([]state.DeliveryHook, 0, len(blocks))
		for _, block := range blocks {
			hook, ok, err := store.DeliveryHook(snapshot.SourceID(block))
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				hooks = append(hooks, hook)
			}
		}
		if len(hooks) == len(blocks) {
			return hooks
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d hooks announced themselves", len(hooks), len(blocks))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// paneText is the pane's text with wrapped lines joined, so a command longer
// than the pane is wide reads as one line.
func paneText(t *testing.T, c Client, session, block snapshot.LiveID) string {
	t.Helper()
	var out struct {
		Content string `json:"content"`
	}
	if err := c.Call(t.Context(), "com.superlogical.terminal.format", map[string]any{"session_id": session, "block_id": block, "args": map[string]any{"format": "text", "unwrap": true}}, &out); err != nil {
		t.Fatal(err)
	}
	return out.Content
}

func waitPaneText(t *testing.T, c Client, session, block snapshot.LiveID, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var text string
	for time.Now().Before(deadline) {
		text = paneText(t, c, session, block)
		if strings.Contains(text, want) {
			return text
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("block %s never showed %q:\n%s", block, want, text)
	return ""
}

// The boundaries the mailbox contract guards: a shell replaced with exec
// keeps its pid and its stale announcement but has no handler, and must be
// left alive and undelivered; an acknowledgement left from another attempt
// is ignored; a request addressed to a hook that is not the one in the pane
// is rejected by the hook itself.
func TestDeliveryHookBoundariesPrivateServer(t *testing.T) {
	for _, shell := range []string{"fish", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s not installed", shell)
			}
			dir := filepath.Join(t.TempDir(), "saved")
			s := newIsolatedServerWithSetup(t, func(s *isolatedServer) { installHook(t, s, shell, dir) })
			leaf := func(label string) createWindow {
				return createWindow{Label: label, Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Label: label, Options: shellOptions{Shell: path, Login: new(bool), CWD: s.root}}}}
			}
			var created createdSession
			mustCall(t, s.client, "session.create", map[string]any{"label": "boundaries", "initial_windows": []createWindow{leaf("replaced"), leaf("stale-ack"), leaf("other-hook")}}, &created)
			blocks := make([]snapshot.LiveID, 3)
			for i := range blocks {
				blocks[i] = created.Windows[i].Blocks[0]
			}
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			hooks := awaitHooks(t, store, blocks)
			mailbox := func(i int) string {
				return filepath.Join(dir, "deliver", strings.TrimPrefix(string(blocks[i]), "block:"))
			}
			// Another attempt's acknowledgement is already in the second
			// mailbox; the third announces a hook id the pane's hook does
			// not have.
			if err := os.WriteFile(filepath.Join(mailbox(1), "ack"), []byte("ffffffffffffffffffffffffffffffff skipped:pending_input\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mailbox(2), "ready"), []byte(fmt.Sprintf("%d %s stale.1\n", hooks[2].PID, shell)), 0600); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			for i, command := range []string{"echo must-not-appear-replaced", "echo typed-after-stale-ack", "echo must-not-appear-other"} {
				writeForeground(t, dir, snapshot.SourceID(blocks[i]), command, 0)
			}
			// The first pane's shell replaces itself without any config, so
			// the hook is gone while the pid and the announcement remain.
			replace := "exec " + path + " --no-config\r"
			if shell == "zsh" {
				replace = "exec " + path + " --no-rcs\r"
			}
			mustCall(t, s.client, "com.superlogical.terminal.write", map[string]any{"session_id": created.ID, "block_id": blocks[0], "args": map[string]any{"data": replace}}, nil)
			deadline := time.Now().Add(10 * time.Second)
			for {
				pane := s.client.paneGate(t.Context(), created.ID, blocks[0])
				promptEnd := map[string]string{"fish": ">", "zsh": "%"}[shell]
				if text := paneText(t, s.client, created.ID, blocks[0]); pane.reason == "" && pane.pid == hooks[0].PID && strings.Count(text, "exec ") == 1 && strings.HasSuffix(strings.TrimSpace(text), promptEnd) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("replacement shell never settled: %+v\n%s", pane, paneText(t, s.client, created.ID, blocks[0]))
				}
				time.Sleep(100 * time.Millisecond)
			}
			time.Sleep(500 * time.Millisecond)
			e, err := s.client.Save(t.Context(), dir, false)
			if err != nil {
				t.Fatal(err)
			}
			target, err := s.client.identity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			latest := make(map[snapshot.SourceID]state.JournalRecord)
			for _, session := range e.Snapshot.Sessions {
				latest[session.ID] = state.JournalRecord{SourceID: session.ID, Outcome: "native", IDMap: identity(t, snapshot.Single(session))}
			}
			store, err = state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			deliveries, changed, err := s.client.deliverNative(t.Context(), store, e, latest, target)
			if err := errors.Join(err, store.Close()); err != nil || !changed || len(deliveries) != 3 {
				t.Fatalf("deliveries: %+v changed=%v %v", deliveries, changed, err)
			}
			byBlock := make(map[snapshot.SourceID]state.Delivery)
			for _, d := range deliveries {
				byBlock[d.BlockID] = d
			}
			// The replaced shell was never delivered to and is still alive at
			// its prompt, with the same pid.
			if d := byBlock[snapshot.SourceID(blocks[0])]; d.Outcome != state.DeliverySkipped || !strings.Contains(d.Reason, "did not claim") && !strings.Contains(d.Reason, "not running") {
				t.Errorf("replaced pane: %+v", d)
			}
			if pane := s.client.paneGate(t.Context(), created.ID, blocks[0]); pane.reason != "" || pane.pid != hooks[0].PID || pane.shell != shell {
				t.Errorf("replaced pane after delivery: %+v, want %s %d alive", pane, shell, hooks[0].PID)
			}
			if text := paneText(t, s.client, created.ID, blocks[0]); strings.Contains(text, "must-not-appear") {
				t.Errorf("replaced pane changed:\n%s", text)
			}
			if d := byBlock[snapshot.SourceID(blocks[1])]; d.Outcome != state.DeliveryTypedOnly {
				t.Errorf("stale-ack pane: %+v", d)
			}
			waitPaneText(t, s.client, created.ID, blocks[1], "echo typed-after-stale-ack")
			if d := byBlock[snapshot.SourceID(blocks[2])]; d.Outcome != state.DeliverySkipped || !strings.Contains(d.Reason, "not the one that announced") {
				t.Errorf("other-hook pane: %+v", d)
			}
			if text := paneText(t, s.client, created.ID, blocks[2]); strings.Contains(text, "must-not-appear") {
				t.Errorf("other-hook pane changed:\n%s", text)
			}
		})
	}
}
