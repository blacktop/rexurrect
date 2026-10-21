package rex

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
	"github.com/blacktop/rexurrect/internal/state"
)

func restoreFixture(t *testing.T, s *isolatedServer) []snapshot.LiveID {
	t.Helper()
	dirs := make([]string, 4)
	for i, name := range []string{"first", "left", "top", "bottom"} {
		dirs[i] = filepath.Join(s.root, name)
		if err := os.Mkdir(dirs[i], 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(s.root, "alias")
	if err := os.Symlink(dirs[1], alias); err != nil {
		t.Fatal(err)
	}
	leaf := func(label, cwd string) createNode {
		return createNode{Block: &createBlock{Flavor: shellFlavor, Label: label, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: cwd}}}
	}
	nested := createNode{Split: &createSplit{Direction: "horizontal", Ratio: 0.37, Before: leaf("left", alias), After: createNode{Split: &createSplit{Direction: "vertical", Ratio: 0.61, Before: leaf("top", dirs[2]), After: leaf("bottom", dirs[3])}}}}
	var sources []snapshot.LiveID
	for range 2 {
		var result createdSession
		mustCall(t, s.client, "session.create", map[string]any{"label": "same-session-label", "initial_windows": []createWindow{{Label: "first", Layout: leaf("first", dirs[0])}, {Label: "work", Layout: nested}}}, &result)
		if len(result.Windows) != 2 || len(result.Windows[1].Blocks) != 3 {
			t.Fatalf("fixture inventory: %+v", result)
		}
		sources = append(sources, result.ID)
		mustCall(t, s.client, "session.focus_block", map[string]any{"session_id": result.ID, "block_id": result.Windows[1].Blocks[2]}, nil)
		mustCall(t, s.client, "session.zoom_block", map[string]any{"session_id": result.ID, "block_id": result.Windows[1].Blocks[2], "zoom": true}, nil)
	}
	return sources
}

func journalFor(t *testing.T, c Client, dir string, e state.Envelope) state.Journal {
	t.Helper()
	id, err := c.identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	j, loadErr := s.LoadJournal(e, id.HostID, id.InstanceID)
	if err := errors.Join(loadErr, s.Close()); err != nil {
		t.Fatal(err)
	}
	return j
}

func rawSessionView(t *testing.T, c Client, id snapshot.LiveID) []byte {
	t.Helper()
	var view json.RawMessage
	mustCall(t, c, "session.view", map[string]any{"session_id": id}, &view)
	return view
}

func listSessions(t *testing.T, c Client) []snapshot.LiveID {
	t.Helper()
	ids, err := c.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func compareRestored(t *testing.T, c Client, e state.Envelope, ids snapshot.IDMap) {
	t.Helper()
	var live []snapshot.LiveID
	for _, session := range e.Snapshot.Sessions {
		live = append(live, ids[session.ID])
	}
	got, report, err := c.Capture(t.Context(), live)
	if err != nil || len(report.Unsupported) != 0 {
		t.Fatalf("capture restored: %+v %v", report, err)
	}
	if err := snapshot.Compare(e.Snapshot, got, ids); err != nil {
		t.Fatal(err)
	}
}

func prepareIntent(t *testing.T, c Client, dir string, e state.Envelope) (state.Envelope, string) {
	t.Helper()
	id, err := c.identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.Publish(e, false)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := state.NewOperationMarker()
	if err != nil {
		t.Fatal(err)
	}
	j := state.NewJournal(e, id.HostID, id.InstanceID)
	if err := errors.Join(s.Record(e, &j, e.Snapshot.Sessions[0].ID, marker, "intent", nil), s.Close()); err != nil {
		t.Fatal(err)
	}
	return e, marker
}

func TestRestoreRecoveryPrivateServer(t *testing.T) {
	requireLoginShell(t)
	s := newIsolatedServer(t)
	sources := restoreFixture(t, s)
	dir := filepath.Join(t.TempDir(), "saved")
	e, err := s.client.Save(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range sources {
		mustCall(t, s.client, "session.destroy", map[string]any{"session_id": id}, nil)
	}
	// A matching display label must not cause an existing session to be adopted.
	unrelated, err := s.client.createSession(t.Context(), e.Snapshot.Sessions[0], e.Snapshot.Sessions[0].Label, nil)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedBefore := rawSessionView(t, s.client, unrelated.ID)
	result, err := s.client.RestoreSaved(t.Context(), dir, 0)
	if err != nil || result.Outcome != "complete" || result.Generation != e.Generation {
		t.Fatalf("restore: %+v %v", result, err)
	}
	compareRestored(t, s.client, e, result.IDMap)
	if !reflect.DeepEqual(journalFor(t, s.client, dir, e).Status(e), result) {
		t.Fatal("journal differs from returned mappings")
	}
	if !bytes.Equal(unrelatedBefore, rawSessionView(t, s.client, unrelated.ID)) {
		t.Fatal("restore changed unrelated session")
	}
	before, files := listSessions(t, s.client), savedFiles(t, dir)
	restoredViews := make(map[snapshot.LiveID][]byte)
	for _, session := range e.Snapshot.Sessions {
		id := result.IDMap[session.ID]
		restoredViews[id] = rawSessionView(t, s.client, id)
	}
	again, err := s.client.RestoreSaved(t.Context(), dir, e.Generation)
	if err != nil || !reflect.DeepEqual(again, result) || !reflect.DeepEqual(before, listSessions(t, s.client)) || !reflect.DeepEqual(files, savedFiles(t, dir)) {
		t.Fatalf("second restore created a session or changed its journal: %+v %v", again, err)
	}
	for id, view := range restoredViews {
		if !bytes.Equal(view, rawSessionView(t, s.client, id)) {
			t.Fatal("second restore changed a completed session")
		}
	}
	status, err := s.client.Status(t.Context(), dir)
	if err != nil || !reflect.DeepEqual(status.Journal, result) {
		t.Fatalf("journal status: %+v %v", status.Journal, err)
	}
	missing := result.IDMap[e.Snapshot.Sessions[1].ID]
	mustCall(t, s.client, "session.destroy", map[string]any{"session_id": missing}, nil)
	before = listSessions(t, s.client)
	again, err = s.client.RestoreSaved(t.Context(), dir, e.Generation)
	if err == nil || !strings.Contains(err.Error(), "recorded complete session "+string(missing)+" is missing") || !reflect.DeepEqual(again, result) {
		t.Fatalf("missing completed session: %+v %v", again, err)
	}
	status, err = s.client.Status(t.Context(), dir)
	if err != nil || status.Generation != e.Generation || status.Journal.Outcome != "complete_missing" || !reflect.DeepEqual(status.Journal.MissingSessions, []snapshot.LiveID{missing}) || !reflect.DeepEqual(status.Journal.IDMap, result.IDMap) {
		t.Fatalf("missing completed session status: %+v %v", status.Journal, err)
	}
	if !reflect.DeepEqual(before, listSessions(t, s.client)) || !reflect.DeepEqual(files, savedFiles(t, dir)) {
		t.Fatal("missing-session restore or status changed sessions or state directory")
	}
	if !bytes.Equal(unrelatedBefore, rawSessionView(t, s.client, unrelated.ID)) {
		t.Fatal("missing-session restore changed unrelated session")
	}
	for _, session := range e.Snapshot.Sessions {
		id := result.IDMap[session.ID]
		if id == missing {
			continue
		}
		if !bytes.Equal(restoredViews[id], rawSessionView(t, s.client, id)) {
			t.Fatal("missing-session restore changed the remaining completed session")
		}
		mustCall(t, s.client, "session.destroy", map[string]any{"session_id": id}, nil)
	}

	// Keep the original generation intact; separate private stores simulate
	// crashes at the intent and acknowledgement boundaries.
	one := e
	one.Snapshot.Sessions = e.Snapshot.Sessions[:1]
	for _, acknowledged := range []bool{false, true} {
		name := "lost-ack"
		if acknowledged {
			name = "partial-map"
		}
		t.Run(name, func(t *testing.T) {
			recoveryDir := filepath.Join(t.TempDir(), "saved")
			saved, marker := prepareIntent(t, s.client, recoveryDir, one)
			created, err := s.client.createSession(t.Context(), saved.Snapshot.Sessions[0], marker, nil)
			if err != nil {
				t.Fatal(err)
			}
			if acknowledged {
				store, err := state.Open(recoveryDir)
				if err != nil {
					t.Fatal(err)
				}
				j, err := store.LoadJournal(saved, saved.HostID, saved.InstanceID)
				if err != nil {
					t.Fatal(err)
				}
				partial := snapshot.IDMap{saved.Snapshot.Sessions[0].ID: created.ID, saved.Snapshot.Sessions[0].Windows[0].ID: created.Windows[0].ID}
				if err := errors.Join(store.Record(saved, &j, saved.Snapshot.Sessions[0].ID, marker, "created", partial), store.Close()); err != nil {
					t.Fatal(err)
				}
			}
			before := listSessions(t, s.client)
			got, err := s.client.RestoreSaved(t.Context(), recoveryDir, 0)
			if err != nil || got.Outcome != "complete" || got.IDMap[saved.Snapshot.Sessions[0].ID] != created.ID || !reflect.DeepEqual(before, listSessions(t, s.client)) {
				t.Fatalf("recover without new creation: %+v %v", got, err)
			}
			compareRestored(t, s.client, saved, got.IDMap)
			if !bytes.Equal(unrelatedBefore, rawSessionView(t, s.client, unrelated.ID)) {
				t.Fatal("recovery changed unrelated session")
			}
			mustCall(t, s.client, "session.destroy", map[string]any{"session_id": created.ID}, nil)
		})
	}
	t.Run("ambiguous", func(t *testing.T) {
		recoveryDir := filepath.Join(t.TempDir(), "saved")
		saved, marker := prepareIntent(t, s.client, recoveryDir, one)
		for range 2 {
			if _, err := s.client.createSession(t.Context(), saved.Snapshot.Sessions[0], marker, nil); err != nil {
				t.Fatal(err)
			}
		}
		before, files := listSessions(t, s.client), savedFiles(t, recoveryDir)
		views := make(map[snapshot.LiveID][]byte)
		for _, id := range before {
			views[id] = rawSessionView(t, s.client, id)
		}
		if _, err := s.client.RestoreSaved(t.Context(), recoveryDir, 0); err == nil || !strings.Contains(err.Error(), "ambiguous operation marker") {
			t.Fatalf("ambiguous recovery: %v", err)
		}
		if !reflect.DeepEqual(before, listSessions(t, s.client)) || !reflect.DeepEqual(files, savedFiles(t, recoveryDir)) {
			t.Fatal("ambiguous recovery changed sessions or state directory")
		}
		for id, view := range views {
			if !bytes.Equal(view, rawSessionView(t, s.client, id)) {
				t.Fatal("ambiguous recovery mutated a session")
			}
		}
	})
}

func TestRestoreRecordedSessionMustStillMatch(t *testing.T) {
	for _, outcome := range []string{"configuring", "complete"} {
		for _, mode := range []string{"missing", "changed"} {
			t.Run(outcome+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				log := filepath.Join(root, "calls")
				c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.view)
    printf 'view %s\n' "$9" >> "$CALLS"
    if [ "$MODE" = missing ]; then exit 1; fi
    printf '%s\n' '{"session_id":"recorded","windows":[{"window_id":"changed"}]}' ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[]}' ;;
  api:session.list)
    if [ "$MODE" = missing ]; then printf '%s\n' '{"sessions":[]}'
    else printf '%s\n' '{"sessions":[{"session_id":"recorded"}]}'; fi ;;
  *) printf 'mutation\n' >> "$CALLS"; exit 1 ;;
esac
`, "CALLS="+log, "MODE="+mode)
				dir := filepath.Join(root, "saved")
				store, err := state.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				e, err := store.Publish(state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "instance", Snapshot: snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{{ID: "pending"}, {ID: "source"}}}, Report: &snapshot.Report{}}, false)
				if err != nil {
					t.Fatal(err)
				}
				j := state.NewJournal(e, e.HostID, e.InstanceID)
				marker, err := state.NewOperationMarker()
				if err != nil {
					t.Fatal(err)
				}
				var ids snapshot.IDMap
				for _, step := range []string{"intent", "created", "configuring", "complete"} {
					if step != "intent" {
						ids = snapshot.IDMap{"source": "recorded"}
					}
					if err := store.Record(e, &j, "source", marker, step, ids); err != nil {
						t.Fatal(err)
					}
					if step == outcome {
						break
					}
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				before := savedFiles(t, dir)
				want := "recorded " + outcome + " session recorded could not be verified"
				if mode == "missing" {
					want = "recorded " + outcome + " session recorded is missing"
				}
				if got, err := c.RestoreSaved(t.Context(), dir, 0); err == nil || !strings.Contains(err.Error(), want) || got.IDMap["source"] != "recorded" {
					t.Fatalf("recorded-session verification: %+v %v", got, err)
				}
				if outcome == "complete" && mode == "missing" {
					status, err := c.Status(t.Context(), dir)
					if err != nil || status.Journal.Outcome != "pending" || !reflect.DeepEqual(status.Journal.MissingSessions, []snapshot.LiveID{"recorded"}) {
						t.Fatalf("pending journal missing-session status: %+v %v", status.Journal, err)
					}
				}
				calls, err := os.ReadFile(log)
				if err != nil || !strings.Contains(string(calls), `view {"session_id":"recorded"}`) || strings.Contains(string(calls), "mutation") || !reflect.DeepEqual(before, savedFiles(t, dir)) {
					t.Fatalf("recorded-session verification missed read-back or changed state: %q %v", calls, err)
				}
			})
		}
	}
}

func TestRestoreUncertainCreateIsNeverRetried(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(root, "creates")
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.list) printf '%s\n' '{"sessions":[]}' ;;
  api:session.create) printf 'create\n' >> "$CALLS"; exit 1 ;;
  *) printf 'unexpected mutation\n' >> "$CALLS"; exit 1 ;;
esac
`, "CALLS="+log)
	dir := filepath.Join(root, "saved")
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := store.Publish(state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "instance", Snapshot: snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{{ID: "source"}}}, Report: &snapshot.Report{}}, false)
	if err := errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	if result, err := c.RestoreSaved(t.Context(), dir, 0); err == nil || result.Outcome != "uncertain" {
		t.Fatalf("failed creation: %+v %v", result, err)
	}
	before := savedFiles(t, dir)
	if _, err := c.RestoreSaved(t.Context(), dir, e.Generation); err == nil || !strings.Contains(err.Error(), "zero live matches") {
		t.Fatalf("zero-match recovery: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil || string(data) != "create\n" || !reflect.DeepEqual(before, savedFiles(t, dir)) {
		t.Fatalf("uncertain create was repeated: %q %v", data, err)
	}
}

func TestRestoreRejectsInvalidStateWithoutMutation(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(root, "mutations")
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  *) printf 'mutation\n' >> "$CALLS"; exit 1 ;;
esac
`, "CALLS="+log)
	dir := filepath.Join(root, "saved")
	if _, err := c.RestoreSaved(t.Context(), dir, 0); !errors.Is(err, state.ErrNoCurrent) {
		t.Fatalf("restore without current: %v", err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := store.Publish(state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "instance", Snapshot: snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{{ID: "source"}}}, Report: &snapshot.Report{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(store.WriteJournal(e, state.NewJournal(e, e.HostID, e.InstanceID)), store.Close()); err != nil {
		t.Fatal(err)
	}
	var path string
	for name := range savedFiles(t, dir) {
		if strings.HasPrefix(name, "restore-") {
			path = filepath.Join(dir, name)
		}
	}
	for _, tc := range []struct {
		name, want string
		data       []byte
	}{
		{"malformed", "decode restore", []byte(`{"format_version":`)},
		{"oversized", "size limit", bytes.Repeat([]byte(" "), state.MaxFileSize+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			before := savedFiles(t, dir)
			if _, err := c.RestoreSaved(t.Context(), dir, 0); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid journal: %v", err)
			}
			if !reflect.DeepEqual(before, savedFiles(t, dir)) {
				t.Fatal("invalid journal changed state directory")
			}
		})
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid state reached a server mutation: %v", err)
	}
}
