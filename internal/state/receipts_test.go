package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

func stateReceipt(n int) receipt.Receipt {
	return receipt.Receipt{Key: receipt.Key{SessionID: "session:01234567-89ab-4cde-8fab-0123456789ab", BlockID: fmt.Sprintf("block:01234567-89ab-4cde-8fab-%012x", n)}, Harness: "codex-team", Conversation: "01234567-89ab-4cde-8fab-0123456789ab", Model: "model-1", Effort: "xhigh"}
}

func TestSnapshotReceiptAndSkipValidation(t *testing.T) {
	s := openStore(t)
	r := stateReceipt(1)
	r.Harness = "claude-team"
	e := fixture()
	e.Snapshot.Sessions = []snapshot.Session{{ID: snapshot.SourceID(r.SessionID), ActiveWindow: "window", Windows: []snapshot.Window{{ID: "window", LayerID: "layer", Focus: snapshot.SourceID(r.BlockID), Layout: snapshot.Node{Block: &snapshot.Block{ID: snapshot.SourceID(r.BlockID), CWD: "/", AgentResume: &r}}}}}}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	e.Snapshot.Version = 1
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "require snapshot version 2") {
		t.Fatalf("receipt accepted in v1: %v", err)
	}
	e.Snapshot.Version = snapshot.Version
	valid := r
	for _, change := range []func(){func() { r.Conversation += ";id" }, func() { r.SessionID = stateReceipt(2).BlockID }, func() { r.BlockID = stateReceipt(2).BlockID }, func() { r.Harness = "unknown;id" }} {
		change()
		if err := e.Validate(); err == nil {
			t.Fatalf("accepted snapshot receipt %+v", r)
		}
		r = valid
	}
	r.Harness = "unknown"
	if err := e.Validate(); err != nil || r.InitialInput() != "" {
		t.Fatalf("future harness metadata: %v", err)
	}
	r = valid
	e, err := s.Publish(e, false)
	if err != nil {
		t.Fatal(err)
	}
	j := NewJournal(e, e.HostID, e.InstanceID)
	marker, err := NewOperationMarker()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(e, &j, snapshot.SourceID(r.SessionID), marker, "intent", nil); err != nil {
		t.Fatal(err)
	}
	if len(j.Status(e).NotResumed) != 1 {
		t.Fatalf("skip missing: %+v", j)
	}
	j.Records[0].NotResumed[0].Harness = "codex"
	if err := s.WriteJournal(e, j); err == nil {
		t.Fatal("accepted forged skip")
	}
}

func TestReceiptStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "saved")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second writer: %v", err)
	}
	r := stateReceipt(1)
	if err := s.SetReceipt("host", r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "receipts.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	if got, err := s.LoadReceipts("host"); err != nil || !reflect.DeepEqual(got, []receipt.Receipt{r}) {
		t.Fatalf("load: %+v %v", got, err)
	}
	if _, err := s.LoadReceipts("other"); err == nil {
		t.Fatal("accepted other host")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := r
	invalid.Model = "model;id"
	if err := s.SetReceipt("host", invalid); err == nil {
		t.Fatal("accepted invalid receipt")
	}
	entries := make([]receipt.Receipt, MaxReceipts+1)
	for i := range entries {
		entries[i] = stateReceipt(i)
	}
	if err := s.WriteReceipts("host", entries); err == nil {
		t.Fatal("accepted excessive count")
	}
	if err := s.WriteReceipts("host", []receipt.Receipt{r, r}); err == nil {
		t.Fatal("accepted duplicate key")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejection changed store")
	}
	for _, miss := range []receipt.ClearGuard{{Harness: "codex"}, {Harness: "codex", Conversation: r.Conversation}, {Harness: r.Harness, Conversation: "00000000-0000-0000-0000-000000000000"}} {
		if err := s.ClearReceipt("host", r.Key, miss); err != nil {
			t.Fatal(err)
		}
		if unchanged, err := os.ReadFile(path); err != nil || !bytes.Equal(before, unchanged) {
			t.Fatalf("guard %+v changed store", miss)
		}
	}
	if err := s.ClearReceipt("host", r.Key, receipt.ClearGuard{Harness: "zsh"}); err == nil {
		t.Fatal("accepted invalid harness guard")
	}
	if err := s.ClearReceipt("host", r.Key, receipt.ClearGuard{Harness: r.Harness, Conversation: r.Conversation}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadReceipts("host"); err != nil || len(got) != 0 {
		t.Fatalf("clear: %+v %v", got, err)
	}
	for _, bad := range [][]byte{[]byte(strings.Replace(string(before), `"harness":`, `"credential":"forbidden","harness":`, 1)), bytes.Repeat([]byte(" "), MaxReceiptsSize+1), append(append([]byte{}, before...), []byte(`{}`)...)} {
		if err := os.WriteFile(path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.LoadReceipts("host"); err == nil {
			t.Fatal("accepted malformed receipt file")
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(before, &decoded); err != nil || len(decoded) != 3 {
		t.Fatalf("store schema: %s %v", before, err)
	}
}
