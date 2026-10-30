package rex

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// identity maps every saved ID of s to itself, the mapping a native restore
// is expected to leave behind.
func identity(t *testing.T, s snapshot.Snapshot) snapshot.IDMap {
	t.Helper()
	inventory, err := s.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	ids := make(snapshot.IDMap, len(inventory))
	for id := range inventory {
		ids[id] = snapshot.LiveID(id)
	}
	return ids
}

// waitNative returns once the restarted server has brought saved back under
// the same IDs with every block running; Rex creates blocks in the background
// after the server is usable. A server that dies meanwhile is reported.
func waitNative(t *testing.T, s *isolatedServer, saved snapshot.Snapshot) error {
	t.Helper()
	var sources []snapshot.LiveID
	for _, session := range saved.Sessions {
		sources = append(sources, snapshot.LiveID(session.ID))
	}
	slices.Sort(sources)
	deadline := time.Now().Add(DefaultAutoWait)
	var last error
	for time.Now().Before(deadline) {
		if err, dead := s.died(); dead {
			return err
		}
		live, err := s.client.List(t.Context())
		if err != nil {
			last = err
		} else if slices.Sort(live); !slices.Equal(live, sources) {
			last = errors.New("sessions differ from the saved set")
		} else if observed, report, err := s.client.Capture(t.Context(), live); err != nil || len(report.CWDSubstitutions) != 0 {
			last = errors.Join(err, errors.New("capture is not yet complete"))
		} else if last = snapshot.Compare(saved, observed, identity(t, saved)); last == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("native restore did not reproduce the saved sessions: %w", last)
}

// restartNative restarts the private server with its snapshot and waits for
// the native restore. Rex 0.0.1793299046 sometimes kills or crashes the
// restarted server while starting restored blocks; that is retried and
// logged, since it happens before rexurrect does anything.
func restartNative(t *testing.T, s *isolatedServer, saved snapshot.Snapshot) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		s.stop(t)
		err := s.tryStart(t)
		if err == nil {
			err = waitNative(t, s, saved)
		}
		if err == nil {
			return
		}
		if attempt == 3 {
			t.Fatalf("native restore failed %d times: %v", attempt, err)
		}
		t.Logf("REX: the restarted server failed during its native restore (attempt %d): %v", attempt, err)
	}
}

func restoreJournal(t *testing.T, dir string, generation uint64, host, instance string) state.JournalStatus {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	e, err := store.LoadGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	j, err := store.LoadJournal(e, host, instance)
	if err != nil {
		t.Fatal(err)
	}
	return j.Status(e)
}

// When Rex restores sessions from its own snapshot, the automatic cycle must
// keep them under their saved IDs, create only what Rex did not bring back,
// and never produce a second copy.
func TestNativeRestorePrivateServer(t *testing.T) {
	requireLoginShell(t)
	s := newIsolatedServerWithSetup(t, func(s *isolatedServer) { s.restore = true })
	dir := filepath.Join(t.TempDir(), "saved")
	sources := restoreFixture(t, s)
	slices.Sort(sources)
	first, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
	if err != nil || first.State != "first_run" || first.SavedGeneration != 1 {
		t.Fatalf("first cycle: %+v %v", first, err)
	}
	saved := loadSaved(t, dir).Envelope.Snapshot
	restartNative(t, s, saved)
	kept, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
	if err != nil || kept.State != "new_instance" || kept.RestoreOutcome != "complete" || kept.Native != 2 || kept.Restored != (snapshot.Recovery{}) || kept.SavedGeneration != 2 || kept.InstanceID == first.InstanceID {
		t.Fatalf("cycle after native restore: %+v %v", kept, err)
	}
	live := listSessions(t, s.client)
	slices.Sort(live)
	if !slices.Equal(live, sources) {
		t.Fatalf("native restore was duplicated or lost: %v, want %v", live, sources)
	}
	journal := restoreJournal(t, dir, 1, kept.HostID, kept.InstanceID)
	inventory, err := saved.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	var unmapped []string
	for id, kind := range inventory {
		if _, ok := journal.IDMap[id]; !ok {
			unmapped = append(unmapped, kind+" "+string(id))
		}
	}
	if journal.Outcome != "complete" || journal.Native != 2 || len(unmapped) != 0 {
		t.Fatalf("native journal %s with %d native, unmapped %v", journal.Outcome, journal.Native, unmapped)
	}
	for old, id := range journal.IDMap {
		if string(old) != string(id) {
			t.Fatalf("native mapping changed %s to %s", old, id)
		}
	}
	if again, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout); err != nil || again.State != "same_instance" || again.Native != 0 {
		t.Fatalf("same-instance cycle after native restore: %+v %v", again, err)
	}
	// A session destroyed after the last save is absent from Rex's snapshot
	// too, so the next restart restores one natively and the cycle creates
	// the other from the saved generation.
	mustCall(t, s.client, "session.destroy", map[string]any{"session_id": sources[1]}, nil)
	saved = loadSaved(t, dir).Envelope.Snapshot
	remaining := saved
	remaining.Sessions = slices.DeleteFunc(slices.Clone(saved.Sessions), func(session snapshot.Session) bool { return session.ID == snapshot.SourceID(sources[1]) })
	restartNative(t, s, remaining)
	mixed, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
	if err != nil || mixed.State != "new_instance" || mixed.RestoreOutcome != "complete" || mixed.Native != 1 || mixed.Restored == (snapshot.Recovery{}) || mixed.SavedGeneration != 3 {
		t.Fatalf("mixed cycle: %+v %v", mixed, err)
	}
	live = listSessions(t, s.client)
	journal = restoreJournal(t, dir, 2, mixed.HostID, mixed.InstanceID)
	created := journal.IDMap[snapshot.SourceID(sources[1])]
	if len(live) != 2 || !slices.Contains(live, sources[0]) || slices.Contains(live, sources[1]) || created == "" || !slices.Contains(live, created) || journal.Native != 1 {
		t.Fatalf("mixed restore: live %v, journal %+v", live, journal)
	}
	t.Logf("kept %d natively restored sessions and created %s for the destroyed one", mixed.Native, created)
}
