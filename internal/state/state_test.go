package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

func fixture() Envelope {
	return Envelope{FormatVersion: FormatVersion, Generation: 1, CapturedAt: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), RexVersion: "test-version", Commit: "test-commit", HostID: "test-host", InstanceID: "test-instance", Snapshot: snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{{ID: "session"}}}, Report: &snapshot.Report{}}
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func encoded(t *testing.T, e any) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = string(data)
	}
	return out
}

func TestLoadRejectsInvalidGenerations(t *testing.T) {
	mutate := func(change func(*Envelope)) []byte {
		e := fixture()
		change(&e)
		return encoded(t, e)
	}
	for _, tc := range []struct {
		name, want string
		data       []byte
	}{
		{"malformed", "decode generation", []byte(`{"format_version":`)},
		{"oversized", "size limit", bytes.Repeat([]byte(" "), MaxFileSize+1)},
		{"envelope-version", "unsupported envelope format version", mutate(func(e *Envelope) { e.FormatVersion++ })},
		{"snapshot-version", "unsupported snapshot version", mutate(func(e *Envelope) { e.Snapshot.Version++ })},
		{"v1-pruning", "receipt pruning requires snapshot version 2", mutate(func(e *Envelope) {
			e.Snapshot.Version = 1
			e.Report.PrunedReceipts = []snapshot.SourceID{snapshot.SourceID(stateReceipt(1).BlockID)}
		})},
		{"snapshot-reference", "invalid active window", mutate(func(e *Envelope) { e.Snapshot.Sessions[0].ActiveWindow = "missing" })},
		{"missing-report", "missing or excessive unsupported report", bytes.ReplaceAll(encoded(t, fixture()), []byte(`,"report":{}`), nil)},
		{"null-report", "missing or excessive unsupported report", mutate(func(e *Envelope) { e.Report = nil })},
		{"report-reference", "invalid unsupported entry", mutate(func(e *Envelope) {
			e.Report.Unsupported = []snapshot.Unsupported{{SessionID: "missing", Kind: "detached", Reason: "test"}}
		})},
		{"metadata", "invalid envelope metadata", mutate(func(e *Envelope) { e.HostID = "" })},
		{"generation", "contains generation 2", mutate(func(e *Envelope) { e.Generation = 2 })},
		{"trailing", "trailing JSON", append(encoded(t, fixture()), []byte(` {}`)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			if _, err := s.Publish(fixture(), false); err != nil {
				t.Fatal(err)
			}
			writeFile(t, s.dir, generationName(1), tc.data)
			before := files(t, s.dir)
			if _, err := s.LoadCurrent(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load error = %v; want %q", err, tc.want)
			}
			if _, err := s.Publish(fixture(), false); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("publication error = %v; want %q", err, tc.want)
			}
			if !reflect.DeepEqual(files(t, s.dir), before) {
				t.Fatal("failed validation changed state")
			}
		})
	}
}

func TestCaptureCWDSubstitutionValidation(t *testing.T) {
	e := fixture()
	e.Snapshot.Sessions[0].ActiveWindow = "window"
	e.Snapshot.Sessions[0].Windows = []snapshot.Window{{ID: "window", LayerID: "layer", Focus: "block", Layout: snapshot.Node{Block: &snapshot.Block{ID: "block", CWD: "/home"}}}}
	valid := snapshot.CWDSubstitution{BlockID: "block", CWD: "/home"}
	e.Report.CWDSubstitutions = []snapshot.CWDSubstitution{valid}
	store := openStore(t)
	written, err := store.Publish(e, false)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCurrent()
	if err != nil || !reflect.DeepEqual(loaded.Envelope, written) {
		t.Fatalf("substitution round trip: %+v %v", loaded, err)
	}
	for _, subs := range [][]snapshot.CWDSubstitution{
		{{BlockID: "window", CWD: "/home"}},
		{{BlockID: "block", CWD: "/other"}},
		{valid, valid},
	} {
		e.Report.CWDSubstitutions = subs
		before := files(t, store.dir)
		if _, err := store.Publish(e, false); err == nil || !strings.Contains(err.Error(), "invalid capture cwd substitution") {
			t.Fatalf("invalid substitution accepted: %+v %v", subs, err)
		}
		if !reflect.DeepEqual(before, files(t, store.dir)) {
			t.Fatal("invalid substitution changed state")
		}
	}
	e.Report.CWDSubstitutions = []snapshot.CWDSubstitution{valid}
	e.Snapshot.Version = 1
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "require snapshot version 2") {
		t.Fatalf("version 1 accepted capture substitution: %v", err)
	}
}

func TestTemporaryAndOrphanFilesAreNotCurrent(t *testing.T) {
	s := openStore(t)
	writeFile(t, s.dir, ".tmp-leftover", encoded(t, fixture()))
	if _, err := s.LoadCurrent(); !errors.Is(err, ErrNoCurrent) {
		t.Fatalf("temporary file became current: %v", err)
	}
	if _, err := s.Publish(fixture(), false); err != nil {
		t.Fatal(err)
	}
	writeFile(t, s.dir, generationName(2), encoded(t, fixture()))
	before := files(t, s.dir)
	if _, err := s.Publish(fixture(), false); err == nil || !strings.Contains(err.Error(), "unpublished generation already exists") {
		t.Fatalf("orphan publication: %v", err)
	}
	current, err := s.LoadCurrent()
	if err != nil || current.Envelope.Generation != 1 || !reflect.DeepEqual(files(t, s.dir), before) {
		t.Fatalf("orphan changed current: %+v %v", current, err)
	}
	writeFile(t, s.dir, "current.json", []byte(`{"generation":".tmp-leftover","last_known_good":0}`))
	if _, err := s.LoadCurrent(); err == nil || !strings.Contains(err.Error(), "decode current.json") {
		t.Fatalf("temporary pointer accepted: %v", err)
	}
}

func TestGenerationRetention(t *testing.T) {
	s := openStore(t)
	var previous Envelope
	var previousBytes string
	for n := uint64(1); n <= 6; n++ {
		e := fixture()
		e.CapturedAt = e.CapturedAt.Add(time.Duration(n) * time.Second)
		written, err := s.Publish(e, false)
		if err != nil {
			t.Fatal(err)
		}
		current, err := s.LoadCurrent()
		if err != nil || written.Generation != n || !reflect.DeepEqual(current.Envelope, written) {
			t.Fatalf("current generation %d: %+v %v", n, current, err)
		}
		retained := files(t, s.dir)
		wantFiles := 3 // lock, pointer, and first generation
		if n > 1 {
			wantFiles = 4
			if current.LastKnownGood == nil || !reflect.DeepEqual(*current.LastKnownGood, previous) || retained[generationName(n-1)] != previousBytes {
				t.Fatalf("generation %d lost or rewrote last-known-good", n)
			}
		}
		if len(retained) != wantFiles {
			t.Fatalf("generation %d retained %d files, want %d", n, len(retained), wantFiles)
		}
		previous, previousBytes = written, retained[generationName(n)]
	}
}

func TestGenerationRetentionPrunesExistingHistory(t *testing.T) {
	s := openStore(t)
	const oldCurrent = 300
	for n := uint64(1); n <= oldCurrent; n++ {
		e := fixture()
		e.Generation = n
		writeFile(t, s.dir, generationName(n), encoded(t, e))
	}
	writeFile(t, s.dir, "current.json", encoded(t, pointer{Generation: oldCurrent, LastKnownGood: oldCurrent - 1}))
	for _, name := range []string{".tmp-leftover", "restore-sentinel.json", "generation-1.json", generationName(0), "generation-18446744073709551616.json", generationName(oldCurrent + 2)} {
		writeFile(t, s.dir, name, []byte("untouched"))
	}
	before := files(t, s.dir)
	invalid := fixture()
	invalid.HostID = "another-host"
	if _, err := s.Publish(invalid, false); err == nil || !reflect.DeepEqual(files(t, s.dir), before) {
		t.Fatalf("failed publication changed retained history: %v", err)
	}
	if _, err := s.Publish(fixture(), false); err != nil {
		t.Fatal(err)
	}
	current, err := s.LoadCurrent()
	if err != nil || current.Envelope.Generation != oldCurrent+1 || current.LastKnownGood == nil || current.LastKnownGood.Generation != oldCurrent {
		t.Fatalf("retained current: %+v %v", current, err)
	}
	for n := uint64(1); n < oldCurrent; n++ {
		delete(before, generationName(n))
	}
	after := files(t, s.dir)
	delete(before, "current.json")
	delete(after, "current.json")
	delete(after, generationName(oldCurrent+1))
	if !reflect.DeepEqual(after, before) {
		t.Fatal("pruning left old generations or changed unrelated files")
	}
}

func TestStateLockExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir)
	if err == nil {
		_ = second.Close()
		t.Error("second lock holder succeeded")
	} else if !strings.Contains(err.Error(), "state is locked") {
		t.Errorf("lock error: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Open(dir)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyAndHostGuards(t *testing.T) {
	s := openStore(t)
	if _, err := s.Publish(fixture(), false); err != nil {
		t.Fatal(err)
	}
	other := fixture()
	other.HostID = "other-host"
	if _, err := s.Publish(other, true); err == nil || !strings.Contains(err.Error(), "different Rex host") {
		t.Fatalf("host guard: %v", err)
	}
	empty := fixture()
	empty.Snapshot.Sessions = nil
	before := files(t, s.dir)
	if _, err := s.Publish(empty, false); err == nil || !strings.Contains(err.Error(), "--allow-empty") {
		t.Fatalf("empty guard: %v", err)
	}
	if !reflect.DeepEqual(files(t, s.dir), before) {
		t.Fatal("guard changed state")
	}
	if _, err := s.Publish(empty, true); err != nil {
		t.Fatal(err)
	}
	current, err := s.LoadCurrent()
	if err != nil || current.Envelope.Generation != 2 || current.LastKnownGood == nil || current.LastKnownGood.Generation != 1 {
		t.Fatalf("last-known-good: %+v %v", current, err)
	}
	if _, err := s.Publish(empty, false); err == nil || !strings.Contains(err.Error(), "--allow-empty") {
		t.Fatalf("empty save discarded nonempty last-known-good: %v", err)
	}
}
