package rex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

func TestAutoUnchangedPrivateServer(t *testing.T) {
	s := newIsolatedServer(t)
	dir := filepath.Join(s.root, "saved")
	var created createdSession
	mustCall(t, s.client, "session.create", map[string]any{"label": "unchanged", "initial_windows": []createWindow{{Label: "shell", Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}}}}, &created)
	cycle := func(want string, generation, saved uint64, reason string) {
		t.Helper()
		out, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
		if err != nil || out.State != want || out.Generation != generation || out.SavedGeneration != saved || out.Reason != reason {
			t.Fatalf("auto: %+v %v", out, err)
		}
	}
	cycle("first_run", 0, 1, "")
	before := savedFiles(t, dir)
	// Files a failed scrollback prune left behind are removed by the next
	// unchanged cycle, which otherwise changes nothing.
	stale := filepath.Join(dir, "scrollback", strings.Repeat("ab", 16))
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".tmp-leftover", strings.Repeat("0", 64) + ".rrsb"} {
		if err := os.WriteFile(filepath.Join(stale, name), []byte("stale"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		cycle("same_instance", 1, 0, "unchanged")
		if entries, err := os.ReadDir(stale); err != nil || len(entries) != 0 {
			t.Fatalf("unchanged cycle left stale scrollback files: %v %v", entries, err)
		}
		if !reflect.DeepEqual(before, savedFiles(t, dir)) {
			t.Fatal("unchanged cycle modified generation or pointer bytes")
		}
	}
	mustCall(t, s.client, "session.set_label", map[string]any{"session_id": created.ID, "label": "changed"}, nil)
	cycle("same_instance", 1, 2, "")
	if current := loadSaved(t, dir); current.Envelope.Snapshot.Sessions[0].Label != "changed" || current.LastKnownGood == nil || current.LastKnownGood.Generation != 1 {
		t.Fatalf("changed capture: %+v", current)
	}
	// An unsupported-only change is still a new capture.
	mustCall(t, s.client, "session.new_block", map[string]any{"session_id": created.ID, "flavor": shellFlavor, "label": "detached", "options": shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}, nil)
	cycle("same_instance", 2, 3, "")
	cycle("same_instance", 3, 0, "unchanged")
	manual, err := s.client.Save(t.Context(), dir, false)
	if err != nil || manual.Generation != 4 {
		t.Fatalf("unchanged manual save: %+v %v", manual, err)
	}
	// An automatic save reports the foreground records it could not attach.
	writeForeground(t, dir, snapshot.SourceID(created.Windows[0].Blocks[0]), strings.Repeat("x", snapshot.MaxForeground+1), 0)
	out, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
	if err != nil || out.SavedGeneration != 5 || len(out.CaptureForegroundSkipped) != 1 || !strings.Contains(out.CaptureForegroundSkipped[0].Reason, "1024 bytes") {
		t.Fatalf("automatic foreground skips: %+v %v", out, err)
	}
	// A records directory that is not private degrades the cycle, never fails
	// it: every block is reported and the directory is left alone.
	if err := os.Chmod(filepath.Join(dir, "foreground"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []uint64{6, 0} {
		out, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
		if err != nil || out.SavedGeneration != generation || len(out.CaptureForegroundSkipped) != 1 || !strings.Contains(out.CaptureForegroundSkipped[0].Reason, "private directory") {
			t.Fatalf("shared records directory: %+v %v", out, err)
		}
	}
}

func TestAutoCyclePrivateServer(t *testing.T) {
	requireLoginShell(t)
	s := newIsolatedServer(t)
	dir := filepath.Join(t.TempDir(), "saved")
	cycle := func(want string, generation, saved uint64) AutoResult {
		t.Helper()
		out, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
		wantReason := ""
		if saved == 0 {
			wantReason = "unchanged"
		}
		if err != nil || out.State != want || out.Generation != generation || out.SavedGeneration != saved || out.Reason != wantReason {
			t.Fatalf("auto: %+v %v", out, err)
		}
		return out
	}
	first := cycle("first_run", 0, 1)
	if e := loadSaved(t, dir).Envelope; e.InstanceID != first.InstanceID || len(e.Snapshot.Sessions) != 0 {
		t.Fatalf("first-run envelope: %+v", e)
	}
	sources := restoreFixture(t, s)
	slices.Sort(sources)
	view, err := s.client.view(t.Context(), sources[0])
	if err != nil {
		t.Fatal(err)
	}
	mustCall(t, s.client, "session.new_layer", map[string]any{"session_id": sources[0], "window_id": view.Windows[0].ID, "bounds": []float64{0.1, 0.1, 0.6, 0.5}, "layout": createNode{Block: &createBlock{Flavor: shellFlavor, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}, "focus": false}, nil)
	mustCall(t, s.client, "session.new_block", map[string]any{"session_id": sources[1], "flavor": shellFlavor, "label": "detached", "options": shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}, nil)
	second := cycle("same_instance", 1, 2)
	if second.InstanceID != first.InstanceID || second.RestoreOutcome != "not_started" {
		t.Fatalf("same-instance decision: %+v", second)
	}
	got := listSessions(t, s.client)
	slices.Sort(got)
	if !slices.Equal(got, sources) {
		t.Fatalf("same-instance cycle restored sessions: %v", got)
	}
	e := loadSaved(t, dir).Envelope
	if len(e.Snapshot.Sessions) != 2 || len(e.Report.Unsupported) != 2 {
		t.Fatalf("S0 fixture was not saved: %+v", e)
	}
	if names, err := filepath.Glob(filepath.Join(dir, "restore-*.json")); err != nil || len(names) != 0 {
		t.Fatalf("same-instance cycle wrote a restore journal: %v %v", names, err)
	}

	s.stop(t)
	s.start(t)
	if got := listSessions(t, s.client); len(got) != 0 {
		t.Fatalf("restart baseline: %v", got)
	}
	restarted := cycle("new_instance", 2, 3)
	if restarted.InstanceID == first.InstanceID || restarted.HostID != first.HostID || restarted.RestoreOutcome != "complete" {
		t.Fatalf("restart decision: %+v", restarted)
	}
	// Plain shells only: the summary says so, and the server's API revision
	// is reported on every cycle.
	if restarted.Restored.Plain == 0 || restarted.Restored.Agents+restarted.Restored.Typed+restarted.Restored.Inert != 0 || restarted.RexAPI.ServerAPIRevision == 0 || first.Restored != (snapshot.Recovery{}) {
		t.Fatalf("restore summary: %+v first %+v", restarted.Restored, first.Restored)
	}
	j := journalFor(t, s.client, dir, e)
	if j.Status(e).Outcome != "complete" {
		t.Fatalf("restore was not completed: %+v", j)
	}
	compareRestored(t, s.client, e, j.Status(e).IDMap)
	current := loadSaved(t, dir)
	if current.Envelope.InstanceID != restarted.InstanceID || current.LastKnownGood == nil || !reflect.DeepEqual(*current.LastKnownGood, e) {
		t.Fatalf("new-instance publication: %+v", current)
	}
	if err := snapshot.Compare(e.Snapshot, current.Envelope.Snapshot, j.Status(e).IDMap); err != nil {
		t.Fatalf("saved before restoration finished: %v", err)
	}
	before := listSessions(t, s.client)
	cycle("same_instance", 3, 0)
	if !slices.Equal(before, listSessions(t, s.client)) || !reflect.DeepEqual(j, journalFor(t, s.client, dir, e)) {
		t.Fatal("following cycle repeated restoration")
	}

	var added createdWindow
	mustCall(t, s.client, "session.new_window", map[string]any{"session_id": before[0], "window_label": "added-after-restore", "layout": createNode{Block: &createBlock{Flavor: shellFlavor, Label: "new", Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}, "focus": false}, &added)
	cycle("same_instance", 3, 4)
	current = loadSaved(t, dir)
	ids, err := current.Envelope.Snapshot.Inventory()
	if err != nil || added.ID == "" || ids[snapshot.SourceID(added.ID)] != "window" {
		t.Fatalf("later window missing from save: %+v %v", added, err)
	}

	for _, differentInstance := range []bool{false, true} {
		for _, outcome := range []string{"pending", "uncertain", "complete_missing"} {
			t.Run(fmt.Sprintf("blocked-%t-%s", differentInstance, outcome), func(t *testing.T) {
				blockedDir := filepath.Join(t.TempDir(), "saved")
				seed := current.Envelope
				if differentInstance {
					seed.InstanceID = first.InstanceID
				}
				seedBlockedJournal(t, blockedDir, seed, current.Envelope.InstanceID, outcome)
				blocked := loadSaved(t, blockedDir).Envelope
				files, sessions, seeded := savedFiles(t, blockedDir), listSessions(t, s.client), journalFor(t, s.client, blockedDir, blocked)
				out, err := s.client.AutoCycle(t.Context(), blockedDir, DefaultAutoWait, privateCycleTimeout)
				want := "same_instance_blocked"
				reason := outcome
				if differentInstance {
					want = "new_instance_unresolved"
					switch outcome {
					case "pending":
						reason = "no longer carries its operation marker"
					case "uncertain":
						reason = "zero live matches"
					}
				}
				if err == nil || out.State != want || out.RestoreOutcome != outcome || out.SavedGeneration != 0 || !strings.Contains(out.Reason, reason) {
					t.Fatalf("unresolved journal: %+v %v", out, err)
				}
				// The fixture sessions are still live under their saved IDs, as
				// after a native Rex restore, so a blocked cycle may still record
				// the sessions it did not have to create. Nothing else may change.
				after := savedFiles(t, blockedDir)
				j := journalFor(t, s.client, blockedDir, blocked)
				for name := range files {
					if strings.HasPrefix(filepath.Base(name), "restore-") {
						delete(files, name)
						delete(after, name)
					}
				}
				if !reflect.DeepEqual(files, after) || !slices.Equal(sessions, listSessions(t, s.client)) {
					t.Fatal("blocked cycle changed state or sessions")
				}
				if len(j.Records) < len(seeded.Records) || !reflect.DeepEqual(j.Records[:len(seeded.Records)], seeded.Records) {
					t.Fatalf("blocked cycle rewrote seeded journal records: %+v", j.Records)
				}
				for _, r := range j.Records[len(seeded.Records):] {
					if r.Outcome != "native" || !slices.Contains(sessions, snapshot.LiveID(r.SourceID)) {
						t.Fatalf("blocked cycle recorded %s for %s", r.Outcome, r.SourceID)
					}
				}
			})
		}
	}

	t.Run("locked", func(t *testing.T) {
		files := savedFiles(t, dir)
		release := holdAutoLock(t, dir)
		defer release()
		marker := filepath.Join(t.TempDir(), "invoked")
		spy := scriptClient(t, "printf 'invoked\\n' > \"$MARKER\"\nexec \"$REAL_REX\" \"$@\"\n", append(s.client.Env, "MARKER="+marker, "REAL_REX="+s.client.Binary)...)
		spy.Socket = s.client.Socket
		out, err := spy.AutoCycle(t.Context(), dir, time.Second, time.Second)
		if !errors.Is(err, state.ErrLocked) || out.State != "locked" || out.SavedGeneration != 0 {
			t.Fatalf("locked cycle: %+v %v", out, err)
		}
		if _, err := spy.Save(t.Context(), dir, false); !errors.Is(err, state.ErrLocked) {
			t.Fatalf("manual save lock: %v", err)
		}
		if _, err := spy.RestoreSaved(t.Context(), dir, 0); !errors.Is(err, state.ErrLocked) {
			t.Fatalf("manual restore lock: %v", err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lock loser touched Rex: %v", err)
		}
		if !reflect.DeepEqual(files, savedFiles(t, dir)) {
			t.Fatal("lock loser changed state")
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		files := savedFiles(t, dir)
		missing := s.client
		missing.Socket = filepath.Join(s.root, "nonexistent")
		for _, limits := range [][2]time.Duration{{100 * time.Millisecond, time.Second}, {time.Second, 100 * time.Millisecond}} {
			started := time.Now()
			out, err := missing.AutoCycle(t.Context(), dir, limits[0], limits[1])
			if !errors.Is(err, context.DeadlineExceeded) || out.State != "unavailable" || out.SavedGeneration != 0 || time.Since(started) > 3*time.Second {
				t.Fatalf("unavailable cycle: %+v %v", out, err)
			}
		}
		if !reflect.DeepEqual(files, savedFiles(t, dir)) {
			t.Fatal("unavailable cycle changed state")
		}
		if _, err := os.Stat(missing.Socket); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unavailable socket was created: %v", err)
		}
	})

	// A restart with unresolved recovery must retain the nonempty current and
	// last-known-good, even though the live instance is entirely empty.
	s.stop(t)
	s.start(t)
	live, err := s.client.whoami(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"uncertain", "complete_missing"} {
		t.Run("empty-restarted-"+outcome, func(t *testing.T) {
			blockedDir := filepath.Join(t.TempDir(), "saved")
			seedBlockedJournal(t, blockedDir, current.Envelope, live.InstanceID, outcome)
			files := savedFiles(t, blockedDir)
			reason := outcome
			if outcome == "uncertain" {
				reason = "zero live matches"
			}
			out, err := s.client.AutoCycle(t.Context(), blockedDir, DefaultAutoWait, privateCycleTimeout)
			if err == nil || out.State != "new_instance_unresolved" || out.RestoreOutcome != outcome || !strings.Contains(out.Reason, reason) || out.SavedGeneration != 0 || len(listSessions(t, s.client)) != 0 || !reflect.DeepEqual(files, savedFiles(t, blockedDir)) {
				t.Fatalf("empty restarted server replaced saved state: %+v %v", out, err)
			}
		})
	}
	// With no journal to block it, automatic publication must still enforce
	// the same empty-server guard as a manual save without --allow-empty.
	t.Run("empty-save-guard", func(t *testing.T) {
		emptyDir := filepath.Join(t.TempDir(), "saved")
		store, err := state.Open(emptyDir)
		if err != nil {
			t.Fatal(err)
		}
		seed := current.Envelope
		seed.InstanceID = live.InstanceID
		_, err = store.Publish(seed, false)
		if err := errors.Join(err, store.Close()); err != nil {
			t.Fatal(err)
		}
		files := savedFiles(t, emptyDir)
		out, err := s.client.AutoCycle(t.Context(), emptyDir, DefaultAutoWait, privateCycleTimeout)
		if err == nil || out.State != "same_instance" || !strings.Contains(out.Reason, "--allow-empty") || out.SavedGeneration != 0 || !reflect.DeepEqual(files, savedFiles(t, emptyDir)) {
			t.Fatalf("empty automatic save: %+v %v", out, err)
		}
	})
	t.Log("initialized, saved without restore, reconstructed after restart, retained later edits, and refused unresolved, unavailable, and locked cycles")
}

func TestAutoCyclePrivateServerRecovery(t *testing.T) {
	requireLoginShell(t)
	s := newIsolatedServer(t)
	restoreFixture(t, s)
	e, err := s.client.Save(t.Context(), filepath.Join(t.TempDir(), "source"), false)
	if err != nil || len(e.Snapshot.Sessions) != 2 {
		t.Fatalf("source save: %+v %v", e, err)
	}
	for _, interruption := range []string{"intent", "uncertain", "complete"} {
		t.Run(interruption, func(t *testing.T) {
			s.stop(t)
			s.start(t)
			target, err := s.client.whoami(t.Context())
			if err != nil || target.HostID != e.HostID || target.InstanceID == e.InstanceID || len(listSessions(t, s.client)) != 0 {
				t.Fatalf("restart baseline: %+v %v", target, err)
			}
			dir := filepath.Join(t.TempDir(), "saved")
			saved, marker := prepareIntent(t, s.client, dir, e)
			first, second := saved.Snapshot.Sessions[0], saved.Snapshot.Sessions[1]
			created, err := s.client.createSession(t.Context(), first, marker, startCommands{})
			if err != nil {
				t.Fatal(err)
			}
			// The first case stops after the real create without recording its
			// result. The others model a lost acknowledgement or a crash after
			// the first session completed, before the second was recorded.
			if interruption != "intent" {
				store, err := state.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				j, err := store.LoadJournal(saved, target.HostID, target.InstanceID)
				if err != nil {
					t.Fatal(err)
				}
				record := func(outcome string, ids snapshot.IDMap) {
					t.Helper()
					if err := store.Record(saved, &j, first.ID, marker, outcome, ids); err != nil {
						t.Fatal(err)
					}
				}
				if interruption == "uncertain" {
					record("uncertain", nil)
				} else {
					ids := make(snapshot.IDMap)
					if err := mapCreated(first, created, ids); err != nil {
						t.Fatal(err)
					}
					record("created", ids)
					view, err := s.client.view(t.Context(), created.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := mapView(first, view, ids); err != nil {
						t.Fatal(err)
					}
					record("configuring", ids)
					mustCall(t, s.client, "session.set_label", map[string]any{"session_id": created.ID, "label": first.Label}, nil)
					if err := s.client.configureSession(t.Context(), first, ids); err != nil {
						t.Fatal(err)
					}
					if err := s.client.verifyRestored(t.Context(), first, ids, false); err != nil {
						t.Fatal(err)
					}
					record("complete", ids)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before := journalFor(t, s.client, dir, saved)
			wantOutcome := "uncertain"
			if interruption == "complete" {
				wantOutcome = "pending"
			}
			if before.Status(saved).Outcome != wantOutcome || before.Latest()[first.ID].Outcome != interruption || before.Latest()[second.ID].Outcome != "" || !slices.Equal(listSessions(t, s.client), []snapshot.LiveID{created.ID}) {
				t.Fatalf("interrupted recovery baseline: %+v", before)
			}
			firstView := rawSessionView(t, s.client, created.ID)
			out, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
			if err != nil || out.State != "new_instance" || out.Generation != saved.Generation || out.SavedGeneration != saved.Generation+1 || out.HostID != target.HostID || out.InstanceID != target.InstanceID || out.RestoreOutcome != "complete" || out.Reason != "" {
				t.Fatalf("resume automatic recovery: %+v %v", out, err)
			}
			j := journalFor(t, s.client, dir, saved)
			status := j.Status(saved)
			if status.Outcome != "complete" || status.IDMap[first.ID] != created.ID || len(j.Records) < len(before.Records) || !reflect.DeepEqual(j.Records[:len(before.Records)], before.Records) {
				t.Fatalf("recovery lost prior progress: %+v", j)
			}
			var firstSteps, secondSteps []string
			for _, record := range j.Records[len(before.Records):] {
				switch record.SourceID {
				case first.ID:
					firstSteps = append(firstSteps, record.Outcome)
				case second.ID:
					secondSteps = append(secondSteps, record.Outcome)
				}
			}
			wantFirst := []string{"recovered", "configuring", "complete"}
			if interruption == "complete" {
				wantFirst = nil
				if !reflect.DeepEqual(firstView, rawSessionView(t, s.client, created.ID)) {
					t.Fatal("resume changed the completed session")
				}
			}
			if !slices.Equal(firstSteps, wantFirst) || !slices.Equal(secondSteps, []string{"intent", "created", "configuring", "complete"}) {
				t.Fatalf("unexpected recovery work: first=%v second=%v", firstSteps, secondSteps)
			}
			wantSessions := []snapshot.LiveID{created.ID, status.IDMap[second.ID]}
			liveSessions := listSessions(t, s.client)
			slices.Sort(wantSessions)
			slices.Sort(liveSessions)
			if !slices.Equal(liveSessions, wantSessions) {
				t.Fatalf("recovery duplicated or lost sessions: %v, want %v", liveSessions, wantSessions)
			}
			compareRestored(t, s.client, saved, status.IDMap)
			current := loadSaved(t, dir)
			if current.Envelope.Generation != out.SavedGeneration || current.Envelope.InstanceID != target.InstanceID || current.LastKnownGood == nil || !reflect.DeepEqual(*current.LastKnownGood, saved) {
				t.Fatalf("recovery publication: %+v", current)
			}
			if err := snapshot.Compare(saved.Snapshot, current.Envelope.Snapshot, status.IDMap); err != nil {
				t.Fatalf("saved before recovery completed: %v", err)
			}
		})
	}
}

func seedBlockedJournal(t *testing.T, dir string, e state.Envelope, instance, outcome string) {
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
	for range 2 {
		e, err = store.Publish(e, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	j := state.NewJournal(e, e.HostID, instance)
	for i, session := range e.Snapshot.Sessions {
		marker, err := state.NewOperationMarker()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Record(e, &j, session.ID, marker, "intent", nil); err != nil {
			t.Fatal(err)
		}
		if outcome == "uncertain" {
			if err := store.Record(e, &j, session.ID, marker, "uncertain", nil); err != nil {
				t.Fatal(err)
			}
			break
		}
		ids := snapshot.IDMap{session.ID: snapshot.LiveID(session.ID)}
		if outcome == "complete_missing" {
			inventory, err := (snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{session}}).Inventory()
			if err != nil {
				t.Fatal(err)
			}
			for id := range inventory {
				ids[id] = snapshot.LiveID(id)
			}
			if i == 0 {
				ids[session.ID] = "missing-completed-session"
			}
		}
		if err := store.Record(e, &j, session.ID, marker, "created", ids); err != nil {
			t.Fatal(err)
		}
		if outcome == "pending" {
			break
		}
		for _, step := range []string{"configuring", "complete"} {
			if err := store.Record(e, &j, session.ID, marker, step, ids); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestAutoLockHolder(t *testing.T) {
	dir := os.Getenv("REXURRECT_TEST_LOCK")
	if dir == "" {
		return
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	_, readErr := io.Copy(io.Discard, os.Stdin)
	if err := errors.Join(readErr, store.Close()); err != nil {
		t.Fatal(err)
	}
}

func holdAutoLock(t *testing.T, dir string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAutoLockHolder$")
	cmd.Env = []string{"REXURRECT_TEST_LOCK=" + dir}
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	release := func() {
		if err := errors.Join(in.Close(), cmd.Wait()); err != nil {
			t.Errorf("lock holder: %v", err)
		}
		cancel()
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "locked\n" {
		release()
		t.Fatalf("lock holder readiness: %q %v", line, err)
	}
	return release
}

func TestAutoRejectsChangedInstanceBeforeSave(t *testing.T) {
	root := t.TempDir()
	count := filepath.Join(root, "called")
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*)
    instance=original
    if test -e "$CALLED"; then instance=changed; fi
    : > "$CALLED"
    printf '{"host_id":"host","instance_id":"%s","rex_version":"version"}\n' "$instance" ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.list) printf '%s\n' '{"sessions":[]}' ;;
  *) exit 1 ;;
esac
`, "CALLED="+count)
	dir := filepath.Join(root, "saved")
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Publish(state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "original", Snapshot: snapshot.Snapshot{Version: snapshot.Version}, Report: &snapshot.Report{}}, false)
	if err := errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	files := savedFiles(t, dir)
	out, err := c.AutoCycle(t.Context(), dir, time.Second, time.Second)
	if err == nil || !strings.Contains(out.Reason, "instance changed") || out.SavedGeneration != 0 || !reflect.DeepEqual(files, savedFiles(t, dir)) {
		t.Fatalf("changed instance was saved: %+v %v", out, err)
	}
}

func TestAutoStateGuards(t *testing.T) {
	for _, want := range []string{"other_host", "unreadable"} {
		t.Run(want, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "called")
			c := scriptClient(t, `
: > "$CALLED"
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"live-host","instance_id":"instance","rex_version":"version"}' ;;
  *) exit 1 ;;
esac
`, "CALLED="+marker)
			dir := filepath.Join(root, "saved")
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Publish(state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "saved-host", InstanceID: "instance", Snapshot: snapshot.Snapshot{Version: snapshot.Version}, Report: &snapshot.Report{}}, false)
			if err := errors.Join(err, store.Close()); err != nil {
				t.Fatal(err)
			}
			if want == "unreadable" {
				if err := os.WriteFile(filepath.Join(dir, "current.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			files := savedFiles(t, dir)
			out, err := c.AutoCycle(t.Context(), dir, time.Second, time.Second)
			if err == nil || out.State != want || out.SavedGeneration != 0 || !reflect.DeepEqual(files, savedFiles(t, dir)) {
				t.Fatalf("state guard: %+v %v", out, err)
			}
			if want == "unreadable" {
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unreadable state reached Rex: %v", err)
				}
			}
		})
	}
}
