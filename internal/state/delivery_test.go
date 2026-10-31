package state

import (
	"reflect"
	"strings"
	"testing"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

const (
	deliverySession    = snapshot.SourceID("session:01a18c00-0000-7000-8000-000000000001")
	deliveryReceipt    = snapshot.SourceID("block:01a18c00-0000-7000-8000-000000000002")
	deliveryForeground = snapshot.SourceID("block:01a18c00-0000-7000-8000-000000000003")
	deliveryPlain      = snapshot.SourceID("block:01a18c00-0000-7000-8000-000000000004")
	resumeInput        = "claude --resume 01a18c00-0000-7000-8000-0000000000aa --effort high"
)

// deliveryFixture is a generation with one receipt block, one foreground
// block, and one block that has nothing to deliver.
func deliveryFixture() Envelope {
	e := fixture()
	window := func(n string, b snapshot.Block) snapshot.Window {
		return snapshot.Window{ID: snapshot.SourceID("window:" + n), LayerID: snapshot.SourceID("layer:" + n), Focus: b.ID, Layout: snapshot.Node{Block: &b}}
	}
	resume := &receipt.Receipt{Key: receipt.Key{SessionID: string(deliverySession), BlockID: string(deliveryReceipt)}, Host: "claude", Command: "claude", Conversation: "01a18c00-0000-7000-8000-0000000000aa", Effort: "high"}
	e.Snapshot.Sessions = []snapshot.Session{{ID: deliverySession, ActiveWindow: "window:a", Windows: []snapshot.Window{
		window("a", snapshot.Block{ID: deliveryReceipt, CWD: "/", AgentResume: resume}),
		window("b", snapshot.Block{ID: deliveryForeground, CWD: "/", Foreground: "make test"}),
		window("c", snapshot.Block{ID: deliveryPlain, CWD: "/"}),
	}}}
	return e
}

func TestDeliveryForUsesReceiptOverForeground(t *testing.T) {
	e := deliveryFixture()
	blocks := e.Snapshot.Sessions[0].Blocks()
	d, ok := DeliveryFor(e, deliverySession, *blocks[0])
	if !ok || d.Kind != DeliveryReceipt || d.Input != resumeInput || d.Generation != e.Generation || strings.Contains(d.Input, "\n") {
		t.Fatalf("receipt delivery: %+v %v", d, ok)
	}
	both := *blocks[0]
	both.Foreground = "make test"
	if d, ok := DeliveryFor(e, deliverySession, both); !ok || d.Kind != DeliveryReceipt {
		t.Fatalf("receipt must win over a foreground record: %+v %v", d, ok)
	}
	if d, ok := DeliveryFor(e, deliverySession, *blocks[1]); !ok || d.Kind != DeliveryForeground || d.Input != "make test" {
		t.Fatalf("foreground delivery: %+v %v", d, ok)
	}
	if _, ok := DeliveryFor(e, deliverySession, *blocks[2]); ok {
		t.Fatal("a block without input has nothing to deliver")
	}
}

func TestDeliveryLogTransitions(t *testing.T) {
	s := openStore(t)
	e := deliveryFixture()
	empty, err := s.LoadDeliveries(e.HostID, e.InstanceID)
	if err != nil || len(empty.Records) != 0 || empty.InstanceID != e.InstanceID || empty.Outcomes() != nil {
		t.Fatalf("missing log must load empty: %+v %v", empty, err)
	}
	l := NewDeliveryLog(e.HostID, e.InstanceID)
	resume := Delivery{Generation: e.Generation, SessionID: deliverySession, BlockID: deliveryReceipt, Kind: DeliveryReceipt, Input: resumeInput, Attempt: attemptA}
	typed := Delivery{Generation: e.Generation, SessionID: deliverySession, BlockID: deliveryForeground, Kind: DeliveryForeground, Input: "make test", Shell: "zsh", Attempt: attemptB}
	noAttempt := resume
	noAttempt.Attempt = ""
	badAttempt := resume
	badAttempt.Attempt = "not-hex"
	rejected := []struct {
		name string
		d    Delivery
	}{
		{"resumed without intent", with(resume, DeliveryResumed, "")},
		{"typed without intent", with(typed, DeliveryTypedOnly, "")},
		{"skipped without reason", with(resume, DeliverySkipped, "")},
		{"uncertain without intent", with(resume, DeliveryUncertain, "no ack")},
		{"intent with reason", with(resume, DeliveryIntent, "why")},
		{"intent without attempt", with(noAttempt, DeliveryIntent, "")},
		{"malformed attempt", with(badAttempt, DeliveryIntent, "")},
		{"skip with an attempt but no intent", with(resume, DeliverySkipped, "busy")},
		{"unknown outcome", with(resume, "delivered", "")},
		{"changed input", with(Delivery{Generation: e.Generation, SessionID: deliverySession, BlockID: deliveryReceipt, Kind: DeliveryReceipt, Input: "rm -rf /", Attempt: attemptA}, DeliveryIntent, "")},
		{"wrong kind", with(Delivery{Generation: e.Generation, SessionID: deliverySession, BlockID: deliveryForeground, Kind: DeliveryReceipt, Input: "make test", Attempt: attemptA}, DeliveryIntent, "")},
		{"wrong generation", with(Delivery{Generation: e.Generation + 1, SessionID: deliverySession, BlockID: deliveryForeground, Kind: DeliveryForeground, Input: "make test", Attempt: attemptA}, DeliveryIntent, "")},
		{"wrong session", with(Delivery{Generation: e.Generation, SessionID: "session:01a18c00-0000-7000-8000-0000000000ee", BlockID: deliveryForeground, Kind: DeliveryForeground, Input: "make test", Attempt: attemptA}, DeliveryIntent, "")},
		{"block without input", with(Delivery{Generation: e.Generation, SessionID: deliverySession, BlockID: deliveryPlain, Kind: DeliveryForeground, Input: ""}, DeliverySkipped, "busy")},
		{"unknown block", with(Delivery{Generation: e.Generation, SessionID: deliverySession, BlockID: "block:01a18c00-0000-7000-8000-0000000000ff", Kind: DeliveryForeground, Input: "x"}, DeliverySkipped, "busy")},
		{"long reason", with(resume, DeliverySkipped, strings.Repeat("r", maxDeliveryReason+1))},
	}
	for _, tc := range rejected {
		if err := s.RecordDelivery(e, &l, tc.d); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
	if len(l.Records) != 0 {
		t.Fatalf("rejected records changed the log: %+v", l.Records)
	}
	if err := s.ResolveDelivery(&l, deliveryReceipt, DeliveryUncertain, "nothing to resolve"); err == nil {
		t.Fatal("resolving a block without an intent was accepted")
	}
	if err := s.RecordDelivery(e, &l, with(resume, DeliveryIntent, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelivery(e, &l, with(resume, DeliveryIntent, "")); err == nil {
		t.Fatal("a second intent was accepted")
	}
	// A foreground command is only ever typed; resumed is for receipts.
	if err := s.RecordDelivery(e, &l, with(typed, DeliveryIntent, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveDelivery(&l, deliveryForeground, DeliveryResumed, ""); err == nil {
		t.Fatal("a foreground command was recorded as resumed")
	}
	if err := s.ResolveDelivery(&l, deliveryForeground, DeliveryTypedOnly, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveDelivery(&l, deliveryReceipt, DeliveryUncertain, "no acknowledgement within 2s"); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{DeliveryIntent, DeliveryResumed, DeliveryTypedOnly, DeliverySkipped, DeliveryUncertain} {
		if err := s.RecordDelivery(e, &l, with(resume, outcome, "again")); err == nil {
			t.Errorf("%s after a terminal outcome was accepted", outcome)
		}
		if err := s.ResolveDelivery(&l, deliveryReceipt, outcome, "again"); err == nil {
			t.Errorf("resolving %s after a terminal outcome was accepted", outcome)
		}
	}
	outcomes := l.Outcomes()
	if len(outcomes) != 2 || outcomes[0].BlockID != deliveryReceipt || outcomes[0].Outcome != DeliveryUncertain || outcomes[0].Input != resumeInput || outcomes[1].Outcome != DeliveryTypedOnly || outcomes[1].Shell != "zsh" || outcomes[1].HostID != e.HostID || outcomes[1].Generation != e.Generation {
		t.Fatalf("outcomes: %+v", outcomes)
	}
	loaded, err := s.LoadDeliveries(e.HostID, e.InstanceID)
	if err != nil || !reflect.DeepEqual(loaded, l) {
		t.Fatalf("reloaded log: %+v %v", loaded, err)
	}
	if other, err := s.LoadDeliveries(e.HostID, "another-instance"); err != nil || len(other.Records) != 0 {
		t.Fatalf("another instance has its own empty log: %+v %v", other, err)
	}
	if _, err := s.LoadDeliveries("other-host", e.InstanceID); err != nil {
		t.Fatalf("another host has its own empty log: %v", err)
	}
	otherHost := e
	otherHost.HostID = "other-host"
	if err := s.RecordDelivery(otherHost, &l, with(typed, DeliverySkipped, "busy")); err == nil {
		t.Fatal("a record from another host's generation was accepted")
	}
	if outcomes[0].Attempt != attemptA || outcomes[1].Attempt != attemptB {
		t.Fatalf("resolved records must keep their attempt: %+v", outcomes)
	}
	// A skip needs no intent: nothing was sent to the shell.
	fresh := NewDeliveryLog(e.HostID, "second-instance")
	if err := s.RecordDelivery(e, &fresh, with(noAttempt, DeliverySkipped, "busy: sleep is running")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelivery(e, &fresh, with(resume, DeliveryIntent, "")); err == nil {
		t.Fatal("an intent after a skip was accepted")
	}
}

// The log belongs to the server instance, not to a generation: a block
// delivered from one generation stays delivered when a later generation is
// restored on the same instance, and an intent from a generation that has
// since been replaced can still be closed.
func TestDeliveryLogSpansGenerations(t *testing.T) {
	s := openStore(t)
	gen1 := deliveryFixture()
	l := NewDeliveryLog(gen1.HostID, "live")
	resume := Delivery{Generation: gen1.Generation, SessionID: deliverySession, BlockID: deliveryReceipt, Kind: DeliveryReceipt, Input: resumeInput}
	typed := Delivery{Generation: gen1.Generation, SessionID: deliverySession, BlockID: deliveryForeground, Kind: DeliveryForeground, Input: "make test", Attempt: attemptA}
	if err := s.RecordDelivery(gen1, &l, with(resume, DeliverySkipped, "no hook")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelivery(gen1, &l, with(typed, DeliveryIntent, "")); err != nil {
		t.Fatal(err)
	}
	gen2 := deliveryFixture()
	gen2.Generation = 2
	gen2.Snapshot.Sessions[0].Windows[1].Layout.Block.Foreground = "make lint"
	loaded, err := s.LoadDeliveries(gen2.HostID, "live")
	if err != nil || !reflect.DeepEqual(loaded, l) {
		t.Fatalf("log must load unchanged for a later generation: %+v %v", loaded, err)
	}
	if loaded.Latest()[deliveryReceipt].Outcome != DeliverySkipped || !loaded.Latest()[deliveryReceipt].Terminal() {
		t.Fatalf("the receipt block's record must still be terminal: %+v", loaded.Latest()[deliveryReceipt])
	}
	again := with(resume, DeliverySkipped, "second attempt")
	again.Generation = 2
	if err := s.RecordDelivery(gen2, &loaded, again); err == nil {
		t.Fatal("a second record for a delivered block on the same instance was accepted")
	}
	// The intent came from generation 1 with "make test"; generation 2 saved
	// a different command for the same block, which must not become a new
	// intent nor silently replace the old one.
	newTyped := with(Delivery{Generation: 2, SessionID: deliverySession, BlockID: deliveryForeground, Kind: DeliveryForeground, Input: "make lint", Attempt: attemptB}, DeliveryIntent, "")
	if err := s.RecordDelivery(gen2, &loaded, newTyped); err == nil {
		t.Fatal("a new intent over an open one was accepted")
	}
	if err := s.ResolveDelivery(&loaded, deliveryForeground, DeliveryUncertain, "interrupted"); err != nil {
		t.Fatal(err)
	}
	closed := loaded.Latest()[deliveryForeground]
	if closed.Generation != 1 || closed.Input != "make test" || closed.Outcome != DeliveryUncertain || closed.Attempt != attemptA {
		t.Fatalf("resolved intent must keep its own generation and input: %+v", closed)
	}
	if err := s.PruneDeliveries(gen1.HostID, "live"); err != nil {
		t.Fatal(err)
	}
	stale := NewDeliveryLog(gen1.HostID, "gone")
	if err := s.RecordDelivery(gen1, &stale, with(resume, DeliverySkipped, "busy")); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneDeliveries(gen1.HostID, "live"); err != nil {
		t.Fatal(err)
	}
	if kept, err := s.LoadDeliveries(gen1.HostID, "live"); err != nil || !reflect.DeepEqual(kept, loaded) {
		t.Fatalf("pruning removed the live instance's log: %+v %v", kept, err)
	}
	if pruned, err := s.LoadDeliveries(gen1.HostID, "gone"); err != nil || len(pruned.Records) != 0 {
		t.Fatalf("pruning kept another instance's log: %+v %v", pruned, err)
	}
}

func with(d Delivery, outcome, reason string) Delivery {
	d.Outcome, d.Reason = outcome, reason
	return d
}
