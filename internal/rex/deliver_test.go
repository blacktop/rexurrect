package rex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// deliveryProcess is what the fake rex answers for a pane's process tree.
type deliveryProcess struct {
	Foreground *paneProcess  `json:"foreground,omitempty"`
	Child      *paneProcess  `json:"child,omitempty"`
	Ancestors  []paneProcess `json:"ancestors,omitempty"`
}

const deliverySession = snapshot.SourceID("session:01a18c00-0000-7000-8000-000000000001")

func deliveryBlock(n int) snapshot.SourceID {
	return snapshot.SourceID("block:01a18c00-0000-7000-8000-0000000000" + string(rune('a'+n/10)) + string(rune('0'+n%10)))
}

// deliveryClient is a fake rex that answers process reads from per-block
// files under responses and fails every other call.
func deliveryClient(t *testing.T, responses string) Client {
	t.Helper()
	return scriptClient(t, `case "$8" in
com.superlogical.terminal.process)
	id=$(printf '%s' "$9" | sed -n 's/.*"block_id":"block:\([^"]*\)".*/\1/p')
	if [ -f "$RESPONSES/$id.json" ]; then cat "$RESPONSES/$id.json"; else echo "process unavailable" >&2; exit 1; fi ;;
*) echo "unexpected call $8" >&2; exit 1 ;;
esac
`, "RESPONSES="+responses)
}

func writeProcess(t *testing.T, responses string, block snapshot.SourceID, process deliveryProcess) {
	t.Helper()
	data, err := json.Marshal(process)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(responses, strings.TrimPrefix(string(block), "block:")+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// deliveryEnvelope is a generation with one window per block and a native
// journal record mapping every saved ID to itself.
func deliveryEnvelope(t *testing.T, generation uint64, blocks []snapshot.Block) (state.Envelope, map[snapshot.SourceID]state.JournalRecord) {
	t.Helper()
	var windows []snapshot.Window
	ids := snapshot.IDMap{deliverySession: snapshot.LiveID(deliverySession)}
	for i := range blocks {
		b := blocks[i]
		w := snapshot.Window{ID: snapshot.SourceID("window:" + string(rune('a'+i))), LayerID: snapshot.SourceID("layer:" + string(rune('a'+i))), Focus: b.ID, Layout: snapshot.Node{Block: &b}}
		windows = append(windows, w)
		for _, id := range []snapshot.SourceID{w.ID, w.LayerID, b.ID} {
			ids[id] = snapshot.LiveID(id)
		}
	}
	e := state.Envelope{FormatVersion: state.FormatVersion, Generation: generation, HostID: "host", InstanceID: "captured-on", Snapshot: snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{{ID: deliverySession, ActiveWindow: windows[0].ID, Windows: windows}}}}
	return e, map[snapshot.SourceID]state.JournalRecord{deliverySession: {SourceID: deliverySession, Outcome: "native", IDMap: ids}}
}

func resumeReceipt(block snapshot.SourceID) *receipt.Receipt {
	return &receipt.Receipt{Key: receipt.Key{SessionID: string(deliverySession), BlockID: string(block)}, Host: "claude", Command: "claude", Conversation: "01a18c00-0000-7000-8000-0000000000aa"}
}

// The gate reads each pane once and records one skip per block with a
// reason that names what it saw; blocks without saved input get no record,
// and a second pass over the same instance records nothing new.
func TestDeliverNativeGate(t *testing.T) {
	responses := t.TempDir()
	c := deliveryClient(t, responses)
	loginChain := func(name string) deliveryProcess {
		return deliveryProcess{Foreground: &paneProcess{Name: name, PID: 20}, Child: &paneProcess{Name: "login", PID: 10}, Ancestors: []paneProcess{{Name: "login", PID: 10}}}
	}
	cases := []struct {
		block   snapshot.Block
		process *deliveryProcess
		shell   string
		reason  string
	}{
		{snapshot.Block{ID: deliveryBlock(1), CWD: "/", AgentResume: resumeReceipt(deliveryBlock(1))}, ptr(loginChain("fish")), "fish", "no delivery hook in this shell; install it with rexurrect hook fish"},
		{snapshot.Block{ID: deliveryBlock(2), CWD: "/", Foreground: "make test"}, ptr(deliveryProcess{Foreground: &paneProcess{Name: "zsh", PID: 20}, Child: &paneProcess{Name: "zsh", PID: 20}}), "zsh", "no delivery hook in this shell; install it with rexurrect hook zsh"},
		{snapshot.Block{ID: deliveryBlock(3), CWD: "/", Foreground: "make test"}, ptr(loginChain("bash")), "bash", "shell bash is not supported; fish runs commands, zsh types them"},
		{snapshot.Block{ID: deliveryBlock(4), CWD: "/", AgentResume: resumeReceipt(deliveryBlock(4))}, ptr(deliveryProcess{Foreground: &paneProcess{Name: "sleep", PID: 30}, Child: &paneProcess{Name: "login", PID: 10}, Ancestors: []paneProcess{{Name: "fish", PID: 20}, {Name: "login", PID: 10}}}), "", "busy: sleep is running"},
		{snapshot.Block{ID: deliveryBlock(5), CWD: "/", Foreground: "make test"}, ptr(deliveryProcess{Foreground: &paneProcess{Name: "zsh", PID: 40}, Child: &paneProcess{Name: "login", PID: 10}, Ancestors: []paneProcess{{Name: "sudo", PID: 30}, {Name: "fish", PID: 20}, {Name: "login", PID: 10}}}), "zsh", "busy: zsh runs under sudo"},
		{snapshot.Block{ID: deliveryBlock(6), CWD: "/", AgentResume: resumeReceipt(deliveryBlock(6))}, nil, "", "pane state unknown: "},
		{snapshot.Block{ID: deliveryBlock(7), CWD: "/", Foreground: "make test"}, ptr(deliveryProcess{Child: &paneProcess{Name: "login", PID: 10}}), "", "pane state unknown: no foreground or child process"},
		// A shell that is not the pane's child, with no chain between them,
		// may hang from anything: Rex leaves the chain out when it is
		// incomplete, so this is unknown, not idle.
		{snapshot.Block{ID: deliveryBlock(8), CWD: "/", Foreground: "make test"}, ptr(deliveryProcess{Foreground: &paneProcess{Name: "fish", PID: 20}, Child: &paneProcess{Name: "login", PID: 10}}), "fish", "pane state unknown: process chain to the pane's child is incomplete"},
		{snapshot.Block{ID: deliveryBlock(9), CWD: "/", Foreground: "make test"}, ptr(deliveryProcess{Foreground: &paneProcess{Name: "fish", PID: 20}}), "", "pane state unknown: no foreground or child process"},
		// A login chain that does not end at the reported child is not the
		// pane's own chain.
		{snapshot.Block{ID: deliveryBlock(10), CWD: "/", Foreground: "make test"}, ptr(deliveryProcess{Foreground: &paneProcess{Name: "fish", PID: 20}, Child: &paneProcess{Name: "login", PID: 10}, Ancestors: []paneProcess{{Name: "login", PID: 11}}}), "fish", "pane state unknown: process chain to the pane's child is incomplete"},
		{snapshot.Block{ID: deliveryBlock(11), CWD: "/"}, ptr(loginChain("fish")), "", ""},
	}
	blocks := make([]snapshot.Block, 0, len(cases))
	for _, tc := range cases {
		blocks = append(blocks, tc.block)
		if tc.process != nil {
			writeProcess(t, responses, tc.block.ID, *tc.process)
		}
	}
	store := openState(t)
	e, latest := deliveryEnvelope(t, 7, blocks)
	target := serverIdentity{HostID: "host", InstanceID: "instance"}
	deliveries, changed, err := c.deliverNative(t.Context(), store, e, latest, target)
	if err != nil || !changed {
		t.Fatalf("first pass: %v changed=%v", err, changed)
	}
	if len(deliveries) != len(cases)-1 {
		t.Fatalf("got %d deliveries for %d blocks with input: %+v", len(deliveries), len(cases)-1, deliveries)
	}
	for i, tc := range cases[:len(cases)-1] {
		d := deliveries[i]
		kind := state.DeliveryForeground
		if tc.block.AgentResume != nil {
			kind = state.DeliveryReceipt
		}
		if d.BlockID != tc.block.ID || d.Kind != kind || d.Outcome != state.DeliverySkipped || d.Shell != tc.shell || !strings.HasPrefix(d.Reason, tc.reason) || d.Generation != 7 || d.InstanceID != "instance" || d.SessionID != deliverySession {
			t.Errorf("block %s: %+v, want shell %q reason %q", tc.block.ID, d, tc.shell, tc.reason)
		}
	}
	files := savedFiles(t, store.Dir())
	var logs []string
	for name := range files {
		if strings.HasPrefix(name, "deliver-") {
			logs = append(logs, name)
		}
	}
	if len(logs) != 1 {
		t.Fatalf("delivery log files: %v", logs)
	}
	// A block already recorded on this instance is never visited again, so
	// the same command cannot be typed twice, even when its pane is now idle.
	writeProcess(t, responses, deliveryBlock(6), deliveryProcess{Foreground: &paneProcess{Name: "fish", PID: 1}, Child: &paneProcess{Name: "fish", PID: 1}})
	again, changed, err := c.deliverNative(t.Context(), store, e, latest, target)
	if err != nil || changed || !reflect.DeepEqual(again, deliveries) || !reflect.DeepEqual(savedFiles(t, store.Dir()), files) {
		t.Fatalf("second pass changed deliveries: %+v changed=%v %v", again, changed, err)
	}
	// Sessions the journal did not record as native, and saved blocks the
	// native record does not map, are not visited: Rex's snapshot no longer
	// holds the pane, so there is nothing to deliver into.
	other := serverIdentity{HostID: "host", InstanceID: "other"}
	created := map[snapshot.SourceID]state.JournalRecord{deliverySession: {SourceID: deliverySession, Outcome: "complete", IDMap: latest[deliverySession].IDMap}}
	if none, changed, err := c.deliverNative(t.Context(), store, e, created, other); err != nil || changed || len(none) != 0 {
		t.Fatalf("deliveries for a created session: %+v %v", none, err)
	}
	partial := map[snapshot.SourceID]state.JournalRecord{deliverySession: {SourceID: deliverySession, Outcome: "native", IDMap: snapshot.IDMap{deliverySession: snapshot.LiveID(deliverySession)}}}
	if none, changed, err := c.deliverNative(t.Context(), store, e, partial, other); err != nil || changed || len(none) != 0 {
		t.Fatalf("deliveries for absent blocks: %+v %v", none, err)
	}
}

// A later generation restored on the same instance adds records only for
// blocks that have none yet, and an intent left by an interrupted run is
// closed as uncertain without reading the pane again.
func TestDeliverNativeAcrossGenerations(t *testing.T) {
	responses := t.TempDir()
	c := deliveryClient(t, responses)
	idle := deliveryProcess{Foreground: &paneProcess{Name: "fish", PID: 20}, Child: &paneProcess{Name: "fish", PID: 20}}
	first := snapshot.Block{ID: deliveryBlock(1), CWD: "/", AgentResume: resumeReceipt(deliveryBlock(1))}
	second := snapshot.Block{ID: deliveryBlock(2), CWD: "/", Foreground: "make test"}
	writeProcess(t, responses, first.ID, idle)
	writeProcess(t, responses, second.ID, idle)
	store := openState(t)
	target := serverIdentity{HostID: "host", InstanceID: "instance"}
	gen1, latest1 := deliveryEnvelope(t, 1, []snapshot.Block{first})
	gen1Deliveries, changed, err := c.deliverNative(t.Context(), store, gen1, latest1, target)
	if err != nil || !changed || len(gen1Deliveries) != 1 || gen1Deliveries[0].Generation != 1 {
		t.Fatalf("generation 1: %+v %v", gen1Deliveries, err)
	}
	// Generation 2 saved the same receipt block plus a new foreground block,
	// and an earlier run left an intent for the new block.
	gen2, latest2 := deliveryEnvelope(t, 2, []snapshot.Block{first, second})
	log, err := store.LoadDeliveries("host", "instance")
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := state.DeliveryFor(gen2, deliverySession, second)
	intent.Outcome = state.DeliveryIntent
	if err := store.RecordDelivery(gen2, &log, intent); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(responses, strings.TrimPrefix(string(second.ID), "block:")+".json")); err != nil {
		t.Fatal(err)
	}
	gen2Deliveries, changed, err := c.deliverNative(t.Context(), store, gen2, latest2, target)
	if err != nil || !changed || len(gen2Deliveries) != 2 {
		t.Fatalf("generation 2: %+v %v", gen2Deliveries, err)
	}
	if !reflect.DeepEqual(gen2Deliveries[0], gen1Deliveries[0]) {
		t.Fatalf("the receipt block was recorded again from generation 2: %+v", gen2Deliveries[0])
	}
	closed := gen2Deliveries[1]
	if closed.BlockID != second.ID || closed.Outcome != state.DeliveryUncertain || closed.Generation != 2 || closed.Input != "make test" || !strings.Contains(closed.Reason, "interrupted") {
		t.Fatalf("interrupted intent: %+v", closed)
	}
	if _, changed, err := c.deliverNative(t.Context(), store, gen2, latest2, target); err != nil || changed {
		t.Fatalf("third pass changed deliveries: %v %v", changed, err)
	}
}

func ptr[T any](v T) *T { return &v }
