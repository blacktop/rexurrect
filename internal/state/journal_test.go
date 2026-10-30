package state

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

func TestJournalBindingAndTransitions(t *testing.T) {
	s := openStore(t)
	e, err := s.Publish(fixture(), false)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := NewOperationMarker()
	if err != nil {
		t.Fatal(err)
	}
	j := NewJournal(e, e.HostID, e.InstanceID)
	if err := s.Record(e, &j, "session", marker, "intent", nil); err != nil {
		t.Fatal(err)
	}
	ids := snapshot.IDMap{"session": "live"}
	if err := s.Record(e, &j, "session", marker, "created", ids); err != nil {
		t.Fatal(err)
	}
	ids["session"] = "changed"
	if j.Records[1].IDMap["session"] != "live" {
		t.Fatal("record aliases a caller-owned map")
	}
	before := files(t, s.dir)
	if err := s.Record(e, &j, "session", marker, "configuring", ids); err == nil || !strings.Contains(err.Error(), "acknowledged mapping") {
		t.Fatalf("changed acknowledged ID: %v", err)
	}
	if !reflect.DeepEqual(before, files(t, s.dir)) {
		t.Fatal("invalid transition changed journal")
	}
	second, err := s.Publish(fixture(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadJournal(second, e.HostID, e.InstanceID); !errors.Is(err, ErrNoJournal) {
		t.Fatalf("reused another generation: %v", err)
	}
	if _, err := s.LoadJournal(e, e.HostID, "new-instance"); !errors.Is(err, ErrNoJournal) {
		t.Fatalf("reused another instance: %v", err)
	}
	missingRecords := NewJournal(e, e.HostID, e.InstanceID)
	missingRecords.Records = nil
	for _, tc := range []struct {
		name, want string
		data       []byte
	}{
		{"malformed", "decode restore", []byte(`{"format_version":`)},
		{"null-records", "binding or record count", encoded(t, missingRecords)},
		{"oversized", "size limit", bytes.Repeat([]byte(" "), MaxFileSize+1)},
		{"unknown-field", "unknown field", bytes.Replace(encoded(t, j), []byte(`"format_version"`), []byte(`"unknown"`), 1)},
		{"trailing", "trailing JSON", append(encoded(t, j), []byte(` {}`)...)},
		{"generation", "binding", bytes.ReplaceAll(encoded(t, j), []byte(`"generation":1`), []byte(`"generation":2`))},
		{"instance", "binding", bytes.ReplaceAll(encoded(t, j), []byte(`"instance_id":"test-instance"`), []byte(`"instance_id":"new-instance"`))},
		{"record-binding", "journal record", bytes.Replace(encoded(t, j), []byte(`"source_session_id":"session"`), []byte(`"source_session_id":"absent"`), 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, s.dir, journalName(e.Generation, e.HostID, e.InstanceID), tc.data)
			if _, err := s.LoadJournal(e, e.HostID, e.InstanceID); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid journal accepted: %v", err)
			}
		})
	}
}

func TestJournalMarkerPrefixes(t *testing.T) {
	marker, err := NewOperationMarker()
	if err != nil {
		t.Fatal(err)
	}
	entropy, ok := strings.CutPrefix(marker, "rexurrect:")
	if !ok || len(entropy) != 32 || entropy != strings.ToLower(entropy) {
		t.Fatalf("unexpected new marker: %q", marker)
	}
	for _, tc := range []struct {
		name, marker string
		valid        bool
	}{
		{"current", marker, true},
		{"legacy", "rex-" + "resurrect:" + entropy, true},
		{"missing-prefix", entropy, false},
		{"wrong-prefix", "other:" + entropy, false},
		{"nested-prefix", "rexurrect:" + "rex-" + "resurrect:" + entropy, false},
		{"short", "rexurrect:" + entropy[:30], false},
		{"invalid-hex", "rexurrect:" + strings.Repeat("z", 32), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			e, err := s.Publish(fixture(), false)
			if err != nil {
				t.Fatal(err)
			}
			j := NewJournal(e, e.HostID, e.InstanceID)
			j.Records = []JournalRecord{{Generation: e.Generation, HostID: e.HostID, InstanceID: e.InstanceID, SourceID: "session", Marker: tc.marker, Outcome: "intent"}}
			writeFile(t, s.dir, journalName(e.Generation, e.HostID, e.InstanceID), encoded(t, j))
			loaded, err := s.LoadJournal(e, e.HostID, e.InstanceID)
			if !tc.valid {
				if err == nil || !strings.Contains(err.Error(), "journal record") {
					t.Fatalf("invalid marker accepted: %v", err)
				}
				return
			}
			if err != nil || loaded.Latest()["session"].Marker != tc.marker {
				t.Fatalf("marker read-back: %+v %v", loaded, err)
			}
			if err := s.Record(e, &loaded, "session", tc.marker, "recovered", snapshot.IDMap{"session": "live"}); err != nil {
				t.Fatalf("continue recovery: %v", err)
			}
			loaded, err = s.LoadJournal(e, e.HostID, e.InstanceID)
			if got := loaded.Latest()["session"]; err != nil || got.Marker != tc.marker || got.Outcome != "recovered" || got.IDMap["session"] != "live" {
				t.Fatalf("recovered marker: %+v %v", loaded, err)
			}
		})
	}
}

func TestJournalCWDSubstitutionValidation(t *testing.T) {
	s := openStore(t)
	input := fixture()
	input.Snapshot.Sessions[0].ActiveWindow = "window"
	input.Snapshot.Sessions[0].Windows = []snapshot.Window{{ID: "window", LayerID: "layer", Focus: "block", Layout: snapshot.Node{Block: &snapshot.Block{ID: "block", CWD: "/missing"}}}}
	e, err := s.Publish(input, false)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := NewOperationMarker()
	if err != nil {
		t.Fatal(err)
	}
	valid := CWDSubstitution{BlockID: "block", SavedCWD: "/missing", RestoredCWD: "/home"}
	for _, tc := range []struct {
		name string
		subs []CWDSubstitution
	}{
		{"wrong-kind", []CWDSubstitution{{BlockID: "window", SavedCWD: "/missing", RestoredCWD: "/home"}}},
		{"wrong-original", []CWDSubstitution{{BlockID: "block", SavedCWD: "/other", RestoredCWD: "/home"}}},
		{"relative", []CWDSubstitution{{BlockID: "block", SavedCWD: "/missing", RestoredCWD: "relative"}}},
		{"unclean", []CWDSubstitution{{BlockID: "block", SavedCWD: "/missing", RestoredCWD: "/a/../home"}}},
		{"unchanged", []CWDSubstitution{{BlockID: "block", SavedCWD: "/missing", RestoredCWD: "/missing"}}},
		{"nul", []CWDSubstitution{{BlockID: "block", SavedCWD: "/missing", RestoredCWD: "/home\x00"}}},
		{"oversized", []CWDSubstitution{{BlockID: "block", SavedCWD: "/missing", RestoredCWD: "/" + strings.Repeat("a", 4096)}}},
		{"duplicate", []CWDSubstitution{valid, valid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := NewJournal(e, e.HostID, e.InstanceID)
			before := files(t, s.dir)
			if err := s.Record(e, &j, "session", marker, "intent", nil, tc.subs...); err == nil || !strings.Contains(err.Error(), "cwd substitution") {
				t.Fatalf("invalid substitution accepted: %v", err)
			}
			if !reflect.DeepEqual(before, files(t, s.dir)) || len(j.Records) != 0 {
				t.Fatal("invalid substitution changed journal")
			}
		})
	}
	j := NewJournal(e, e.HostID, e.InstanceID)
	subs := []CWDSubstitution{valid}
	if err := s.Record(e, &j, "session", marker, "intent", nil, subs...); err != nil {
		t.Fatal(err)
	}
	subs[0].RestoredCWD = "/changed"
	ids := snapshot.IDMap{"session": "live"}
	before := files(t, s.dir)
	if err := s.Record(e, &j, "session", marker, "created", ids, valid); err == nil || !reflect.DeepEqual(before, files(t, s.dir)) {
		t.Fatalf("substitution accepted after intent: %v", err)
	}
	if err := s.Record(e, &j, "session", marker, "created", ids); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadJournal(e, e.HostID, e.InstanceID)
	if err != nil || !reflect.DeepEqual(loaded.Status(e).CWDSubstitutions, []CWDSubstitution{valid}) {
		t.Fatalf("valid additive journal field did not round-trip: %+v %v", loaded, err)
	}
}

// A native record says Rex's own snapshot brought the session back: it keeps
// every saved ID, resolves the session without a create, and is counted.
func TestJournalNativeRecords(t *testing.T) {
	s := openStore(t)
	e, err := s.Publish(fixture(), false)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := NewOperationMarker()
	if err != nil {
		t.Fatal(err)
	}
	j := NewJournal(e, e.HostID, e.InstanceID)
	if status := j.Status(e); status.Outcome != "pending" || status.Native != 0 {
		t.Fatalf("empty journal: %+v", status)
	}
	if err := s.Record(e, &j, "session", marker, "native", snapshot.IDMap{"session": "elsewhere"}); err == nil || !strings.Contains(err.Error(), "keep the saved IDs") {
		t.Fatalf("native record with a changed ID: %v", err)
	}
	if err := s.Record(e, &j, "session", marker, "native", snapshot.IDMap{"session": "session"}); err != nil {
		t.Fatal(err)
	}
	status := j.Status(e)
	if status.Outcome != "complete" || status.Native != 1 || status.IDMap["session"] != "session" {
		t.Fatalf("native status: %+v", status)
	}
	for _, outcome := range []string{"intent", "created", "complete", "native"} {
		if err := s.Record(e, &j, "session", marker, outcome, snapshot.IDMap{"session": "session"}); err == nil {
			t.Errorf("%s after native was accepted", outcome)
		}
	}
	loaded, err := s.LoadJournal(e, e.HostID, e.InstanceID)
	if err != nil || !reflect.DeepEqual(loaded.Status(e), status) {
		t.Fatalf("reloaded native journal: %+v %v", loaded, err)
	}
	intent := NewJournal(e, e.HostID, e.InstanceID)
	if err := s.Record(e, &intent, "session", marker, "intent", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(e, &intent, "session", marker, "native", snapshot.IDMap{"session": "session"}); err == nil {
		t.Fatal("native after intent was accepted")
	}
}
