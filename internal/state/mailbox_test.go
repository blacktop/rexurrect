package state

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

const (
	attemptA = "0123456789abcdef0123456789abcdef"
	attemptB = "fedcba9876543210fedcba9876543210"
)

// The mailbox is the hook's directory: rexurrect reads what the hook
// announced, publishes requests bound to an attempt and a hook id only into
// a private mailbox that exists, revokes a request only when it is still
// unclaimed, matches acknowledgements by attempt, and prunes the mailboxes
// of dead blocks once they are old enough.
func TestDeliveryMailbox(t *testing.T) {
	s := openStore(t)
	block := deliveryReceipt
	uuid := strings.TrimPrefix(string(block), "block:")
	if _, ok, err := s.DeliveryHook(block); err != nil || ok {
		t.Fatalf("hook without a mailbox: ok=%v %v", ok, err)
	}
	if err := s.WriteDeliveryRequest(block, attemptA, "4242.1", DeliveryModeRun, "claude --resume x"); err == nil {
		t.Fatal("a request without a mailbox was written")
	}
	if revoked, err := s.RevokeDeliveryRequest(block); err != nil || revoked {
		t.Fatalf("revoking without a mailbox proved something: revoked=%v %v", revoked, err)
	}
	if _, _, err := s.DeliveryHook("not-a-block"); err == nil {
		t.Fatal("an invalid block ID named a mailbox")
	}
	attempt, err := NewDeliveryAttempt()
	if err != nil || !ValidDeliveryAttempt(attempt) || ValidDeliveryAttempt("short") || ValidDeliveryAttempt(strings.ToUpper(attempt)) {
		t.Fatalf("attempt ids: %q %v", attempt, err)
	}
	parent := filepath.Join(s.Dir(), "deliver")
	box := filepath.Join(parent, uuid)
	if err := os.MkdirAll(box, 0700); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.DeliveryHook(block); err != nil || ok {
		t.Fatalf("mailbox without an announcement: ok=%v %v", ok, err)
	}
	for _, bad := range []string{"garbage\n", "4242 fish\n", "4242 fish bad id\n", "0 fish 1.2\n", "4242 fish " + strings.Repeat("x", 129) + "\n"} {
		if err := os.WriteFile(filepath.Join(box, "ready"), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.DeliveryHook(block); err == nil {
			t.Errorf("announcement %q was accepted", bad)
		}
	}
	if err := os.WriteFile(filepath.Join(box, "ready"), []byte("4242 fish 4242.17\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hook, ok, err := s.DeliveryHook(block)
	if err != nil || !ok || hook != (DeliveryHook{PID: 4242, Shell: "fish", ID: "4242.17"}) {
		t.Fatalf("announcement: %+v ok=%v %v", hook, ok, err)
	}
	for _, bad := range []struct{ attempt, hook, mode, command string }{{"short", hook.ID, DeliveryModeRun, "x"}, {attemptA, "bad id", DeliveryModeRun, "x"}, {attemptA, hook.ID, "paste", "x"}, {attemptA, hook.ID, DeliveryModeRun, ""}, {attemptA, hook.ID, DeliveryModeRun, "a\nb"}, {attemptA, hook.ID, DeliveryModeType, strings.Repeat("x", maxDeliveryInput+1)}} {
		if err := s.WriteDeliveryRequest(block, bad.attempt, bad.hook, bad.mode, bad.command); err == nil {
			t.Errorf("request %+v was written", bad)
		}
	}
	if err := s.WriteDeliveryRequest(block, attemptA, hook.ID, DeliveryModeType, "echo typed"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(box, "request")); err != nil || string(data) != attemptA+" 4242.17 type\necho typed\n" {
		t.Fatalf("request file: %q %v", data, err)
	}
	// Acknowledgements name their attempt; a malformed one is an error, not
	// an answer.
	if _, ok, err := s.DeliveryAck(block); err != nil || ok {
		t.Fatalf("acknowledgement before the hook answered: ok=%v %v", ok, err)
	}
	for _, bad := range []string{"delivered:run\n", "short delivered:run\n", attemptA + "\n"} {
		if err := os.WriteFile(filepath.Join(box, "ack"), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.DeliveryAck(block); err == nil {
			t.Errorf("acknowledgement %q was accepted", bad)
		}
	}
	if err := os.WriteFile(filepath.Join(box, "ack"), []byte(attemptB+" skipped:pending_input\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if ack, ok, err := s.DeliveryAck(block); err != nil || !ok || ack != (DeliveryAck{Attempt: attemptB, Outcome: "skipped:pending_input"}) {
		t.Fatalf("acknowledgement: %+v ok=%v %v", ack, ok, err)
	}
	// Revoking succeeds exactly once for an unclaimed request; a claimed
	// (renamed) request cannot be revoked.
	if revoked, err := s.RevokeDeliveryRequest(block); err != nil || !revoked {
		t.Fatalf("revoking an unclaimed request: revoked=%v %v", revoked, err)
	}
	if revoked, err := s.RevokeDeliveryRequest(block); err != nil || revoked {
		t.Fatalf("revoking an absent request: revoked=%v %v", revoked, err)
	}
	if err := s.WriteDeliveryRequest(block, attemptA, hook.ID, DeliveryModeType, "echo typed"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(box, "request"), filepath.Join(box, "claimed."+hook.ID)); err != nil {
		t.Fatal(err)
	}
	if revoked, err := s.RevokeDeliveryRequest(block); err != nil || revoked {
		t.Fatalf("revoking a claimed request: revoked=%v %v", revoked, err)
	}
	if _, ok, err := s.DeliveryFIFO(block); err != nil || ok {
		t.Fatalf("fifo before the hook made one: ok=%v %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(box, "fifo"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DeliveryFIFO(block); err == nil {
		t.Fatal("a plain file was accepted as the fifo")
	}
	if err := os.Remove(filepath.Join(box, "fifo")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(box, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if path, ok, err := s.DeliveryFIFO(block); err != nil || !ok || path != filepath.Join(box, "fifo") {
		t.Fatalf("fifo: %q ok=%v %v", path, ok, err)
	}
	// A mailbox that is not private is refused rather than used.
	if err := os.Chmod(box, 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DeliveryHook(block); err == nil {
		t.Fatal("a world-readable mailbox was used")
	}
	if err := os.Chmod(box, 0700); err != nil {
		t.Fatal(err)
	}
	// Pruning keeps live blocks and fresh mailboxes, and removes the rest.
	dead := filepath.Join(parent, strings.TrimPrefix(string(deliveryForeground), "block:"))
	if err := os.MkdirAll(dead, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * mailboxGrace)
	for _, path := range []string{box, dead} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	fresh := filepath.Join(parent, strings.TrimPrefix(string(deliveryPlain), "block:"))
	if err := os.MkdirAll(fresh, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneDeliveryMailboxes(map[snapshot.SourceID]bool{block: true}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{box: true, dead: false, fresh: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("after pruning, %s exists=%v, want %v", filepath.Base(path), err == nil, want)
		}
	}
}
