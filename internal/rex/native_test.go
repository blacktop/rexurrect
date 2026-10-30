package rex

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

// A session Rex has listed but not yet populated keeps a stable, empty
// shape. The wait must not treat that quiet as finished and record the
// session by its ID alone; it keeps polling until the caller gives up. A
// saved session that has no panes is the one shape an empty live session
// can legitimately settle into.
func TestAwaitNativeUnpopulatedSession(t *testing.T) {
	c := scriptClient(t, `
case "$6:$8" in
  api:session.view) printf '%s\n' '{"session_id":"session:a","label":"bare","windows":[]}' ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[]}' ;;
  *) exit 1 ;;
esac
`)
	session := snapshot.Session{ID: "session:a", Label: "bare", ActiveWindow: "window:a", Windows: []snapshot.Window{{ID: "window:a", LayerID: "layer:a", Focus: "block:a", Layout: snapshot.Node{Block: &snapshot.Block{ID: "block:a", CWD: "/"}}}}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*nativeSettle)
	defer cancel()
	ids, _, err := c.awaitNative(ctx, session)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an unpopulated session settled: ids %v, err %v", ids, err)
	}
	started := time.Now()
	ids, _, err = c.awaitNative(t.Context(), snapshot.Session{ID: "session:a", Label: "bare"})
	if err != nil || !maps.Equal(ids, snapshot.IDMap{"session:a": "session:a"}) || time.Since(started) < nativeSettle {
		t.Fatalf("an empty saved session did not settle: ids %v, err %v after %s", ids, err, time.Since(started))
	}
}

// restartNative restarts the private server with its snapshot, waits only
// until Rex lists the sessions in want, and runs the automatic cycle while
// Rex may still be starting their blocks, which the restore itself must wait
// for. Rex 0.0.1793299046 sometimes kills or crashes the restarted server
// while starting restored blocks; that is retried and logged, since it
// happens before rexurrect does anything.
func restartNative(t *testing.T, s *isolatedServer, dir string, want []snapshot.LiveID) AutoResult {
	t.Helper()
	for attempt := 1; ; attempt++ {
		s.stop(t)
		err := s.tryStart(t)
		if err == nil {
			err = awaitListed(t, s, want)
		}
		var out AutoResult
		if err == nil {
			out, err = s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
			err = errors.Join(err, s.died())
		}
		if err == nil {
			return out
		}
		if s.died() == nil && s.cmd != nil {
			t.Fatalf("cycle after native restore: %+v %v", out, err)
		}
		if attempt == 3 {
			t.Fatalf("native restore failed %d times: %v", attempt, err)
		}
		t.Logf("REX: the restarted server failed during its native restore (attempt %d): %v", attempt, err)
	}
}

// awaitListed waits until the private server lists exactly want, or reports
// that it died or ran out of time.
func awaitListed(t *testing.T, s *isolatedServer, want []snapshot.LiveID) error {
	t.Helper()
	want = slices.Sorted(slices.Values(want))
	deadline := time.Now().Add(DefaultAutoWait)
	for {
		live, err := s.client.List(t.Context())
		slices.Sort(live)
		if err == nil && slices.Equal(live, want) {
			return nil
		}
		if dead := s.died(); dead != nil {
			return dead
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("saved sessions were not listed: %v %v", live, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
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
	gen1 := loadSaved(t, dir).Envelope
	saved := gen1.Snapshot
	inventory, err := saved.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	// A pane added after the save comes back with Rex's snapshot too; the
	// native session must still count as ready and the new pane is saved.
	var extra createdWindow
	mustCall(t, s.client, "session.new_window", map[string]any{"session_id": sources[0], "window_label": "added-after-save", "layout": createNode{Block: &createBlock{Flavor: shellFlavor, Label: "extra", Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}, "focus": false}, &extra)
	// A window closed after the save is absent from Rex's snapshot too; the
	// native session must still count as ready and the deletion is saved.
	second := saved.Sessions[slices.IndexFunc(saved.Sessions, func(session snapshot.Session) bool { return session.ID == snapshot.SourceID(sources[1]) })]
	closed := second.Windows[0]
	mustCall(t, s.client, "session.close_window", map[string]any{"session_id": sources[1], "window_id": closed.ID}, nil)
	deleted, err := snapshot.Single(snapshot.Session{ID: second.ID, ActiveWindow: closed.ID, Windows: []snapshot.Window{closed}}).Inventory()
	if err != nil {
		t.Fatal(err)
	}
	delete(deleted, second.ID)
	if len(deleted) < 3 {
		t.Fatalf("closed window inventory: %v", deleted)
	}
	kept := restartNative(t, s, dir, sources)
	if kept.State != "new_instance" || kept.RestoreOutcome != "complete" || kept.Native != 2 || kept.Restored != (snapshot.Recovery{}) || kept.SavedGeneration != 2 || kept.InstanceID == first.InstanceID {
		t.Fatalf("cycle after native restore: %+v", kept)
	}
	// Every block of both sessions was saved, so the restore waited for Rex
	// to finish starting them before recording the sessions as native.
	after := loadSaved(t, dir).Envelope.Snapshot
	afterIDs, err := after.Inventory()
	if err != nil || len(after.Sessions) != 2 || afterIDs[snapshot.SourceID(extra.ID)] != "window" {
		t.Fatalf("generation 2 lacks the window added after the save: %v %+v", err, after)
	}
	want := make(snapshot.IDMap, len(inventory))
	for id := range inventory {
		if _, gone := deleted[id]; gone {
			if afterIDs[id] != "" {
				t.Fatalf("generation 2 still holds closed resource %s", id)
			}
			continue
		}
		if afterIDs[id] == "" {
			t.Fatalf("generation 2 lost restored resource %s", id)
		}
		want[id] = snapshot.LiveID(id)
	}
	// Generation 2 has no journal on this instance; generation 1 holds the
	// cycle's native records. Once the live layout moves on, an explicit
	// restore of either must be refused without creating copies, writing a
	// journal, or touching the one that exists.
	mustCall(t, s.client, "session.new_window", map[string]any{"session_id": sources[0], "window_label": "added-after-generation-2", "layout": createNode{Block: &createBlock{Flavor: shellFlavor, Label: "later", Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}, "focus": false}, nil)
	liveCount := len(listSessions(t, s.client))
	// Destroying the session recovers the saved layout only while no native
	// record exists for it, so only the refusal for generation 2 may say so.
	for _, generation := range []uint64{2, 1} {
		before := savedFiles(t, dir)
		_, err := s.client.RestoreSaved(t.Context(), dir, generation)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("differs from generation %d", generation)) || strings.Contains(err.Error(), "destroy") != (generation == 2) {
			t.Fatalf("explicit restore of generation %d: %v", generation, err)
		}
		if n := len(listSessions(t, s.client)); n != liveCount {
			t.Fatalf("refused restore of generation %d changed the live sessions: %d, want %d", generation, n, liveCount)
		}
		if !reflect.DeepEqual(before, savedFiles(t, dir)) {
			t.Fatalf("refused restore of generation %d changed the state directory", generation)
		}
	}
	live := listSessions(t, s.client)
	slices.Sort(live)
	if !slices.Equal(live, sources) {
		t.Fatalf("native restore was duplicated or lost: %v, want %v", live, sources)
	}
	journal := journalFor(t, s.client, dir, gen1).Status(gen1)
	if journal.Outcome != "complete" || journal.Native != 2 || !maps.Equal(journal.IDMap, want) {
		t.Fatalf("native journal %s with %d native: %v, want %v", journal.Outcome, journal.Native, journal.IDMap, want)
	}
	if again, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout); err != nil || again.State != "same_instance" || again.Native != 0 || again.SavedGeneration != 3 {
		t.Fatalf("same-instance cycle after native restore: %+v %v", again, err)
	}
	// The current generation matches what Rex restored, so an explicit
	// restore of it keeps both sessions without creating anything.
	if current, err := s.client.RestoreSaved(t.Context(), dir, 0); err != nil || current.Outcome != "complete" || current.Native != 2 || len(listSessions(t, s.client)) != liveCount {
		t.Fatalf("explicit restore of the current generation: %+v %v", current, err)
	}
	// A session destroyed after the last save is absent from Rex's snapshot
	// too, so the next restart restores one natively and the cycle creates
	// the other from the saved generation.
	mustCall(t, s.client, "session.destroy", map[string]any{"session_id": sources[1]}, nil)
	gen3 := loadSaved(t, dir).Envelope
	mixed := restartNative(t, s, dir, sources[:1])
	if mixed.State != "new_instance" || mixed.RestoreOutcome != "complete" || mixed.Native != 1 || mixed.Restored == (snapshot.Recovery{}) || mixed.SavedGeneration != 4 {
		t.Fatalf("mixed cycle: %+v", mixed)
	}
	live = listSessions(t, s.client)
	journal = journalFor(t, s.client, dir, gen3).Status(gen3)
	created := journal.IDMap[snapshot.SourceID(sources[1])]
	if len(live) != 2 || !slices.Contains(live, sources[0]) || slices.Contains(live, sources[1]) || created == "" || !slices.Contains(live, created) || journal.Native != 1 {
		t.Fatalf("mixed restore: live %v, journal %+v", live, journal)
	}
	t.Logf("kept %d natively restored sessions and created %s for the destroyed one", mixed.Native, created)

	// A native record is only as good as the session behind it: once that
	// session is gone, status reports it missing and a retry refuses to
	// finish around it, exactly as for a session the restore created.
	vanished := filepath.Join(t.TempDir(), "saved")
	current := loadSaved(t, dir).Envelope
	seedBlockedJournal(t, vanished, current, mixed.InstanceID, "native")
	gone := snapshot.LiveID(current.Snapshot.Sessions[len(current.Snapshot.Sessions)-1].ID)
	mustCall(t, s.client, "session.destroy", map[string]any{"session_id": gone}, nil)
	status, err := s.client.Status(t.Context(), vanished)
	if err != nil || status.Journal.Outcome != "complete_missing" || !slices.Equal(status.Journal.MissingSessions, []snapshot.LiveID{gone}) {
		t.Fatalf("status after a native session vanished: %+v %v", status.Journal, err)
	}
	before := len(listSessions(t, s.client))
	if _, err := s.client.RestoreSaved(t.Context(), vanished, 0); err == nil || !strings.Contains(err.Error(), "recorded native session") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("retry over a vanished native session: %v", err)
	}
	if blocked, err := s.client.AutoCycle(t.Context(), vanished, DefaultAutoWait, privateCycleTimeout); err == nil || blocked.RestoreOutcome != "complete_missing" || blocked.SavedGeneration != 0 {
		t.Fatalf("automatic cycle over a vanished native session: %+v %v", blocked, err)
	}
	if len(listSessions(t, s.client)) != before {
		t.Fatal("a refused retry changed the live sessions")
	}
}
