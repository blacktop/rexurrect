package rex

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// RestoreSaved restores a published generation (zero selects current). The
// state lock covers journal read-back, every mutation, and final verification.
func (c Client) RestoreSaved(ctx context.Context, dir string, generation uint64) (out state.JournalStatus, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	store, err := state.Open(dir)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	e, err := store.LoadGeneration(generation)
	if err != nil {
		return out, err
	}
	target, err := c.identity(ctx)
	if err != nil {
		return out, err
	}
	return c.restoreSaved(ctx, store, e, target, matchGeneration)
}

// nativePolicy says what a restore does with a saved session that Rex has
// already brought back under the same ID.
type nativePolicy int

const (
	// acceptLiveState keeps whatever Rex restored: its snapshot is newer
	// than the saved generation, so later edits and deletions are truth.
	acceptLiveState nativePolicy = iota
	// matchGeneration keeps the live session only if it equals the selected
	// generation; an explicit restore of an older generation must not report
	// completion over a different layout.
	matchGeneration
)

func (c Client) restoreSaved(ctx context.Context, store *state.Store, e state.Envelope, target serverIdentity, policy nativePolicy) (out state.JournalStatus, err error) {
	if target.HostID != e.HostID {
		return out, errors.New("state belongs to a different Rex host")
	}
	j, err := store.LoadJournal(e, target.HostID, target.InstanceID)
	fresh := errors.Is(err, state.ErrNoJournal)
	if fresh {
		j, err = state.NewJournal(e, target.HostID, target.InstanceID), nil
	}
	if err != nil {
		return out, err
	}
	out = j.Status(e)
	var scrollbackSkipped []snapshot.Skip
	var recovery snapshot.Recovery
	var deliveries []state.Delivery
	defer func() {
		acknowledged := out.IDMap
		out = j.Status(e)
		maps.Copy(out.IDMap, acknowledged)
		out.ScrollbackSkipped = scrollbackSkipped
		out.Recovery = recovery
		out.Deliveries = deliveries
	}()

	latest := j.Latest()
	recordedCWD := make(map[snapshot.SourceID]string)
	for _, sub := range out.CWDSubstitutions {
		recordedCWD[sub.BlockID] = sub.RestoredCWD
	}
	if err := c.verifyRecorded(ctx, e, latest, recordedCWD, policy); err != nil {
		return out, err
	}
	// Resolve every outstanding create before allowing any new mutation. Zero
	// is inconclusive: a timed-out request may still be running on the server.
	recovered := make(map[snapshot.SourceID]snapshot.LiveID)
	for source, r := range latest {
		if r.Outcome != "intent" && r.Outcome != "uncertain" {
			continue
		}
		id, err := c.findOperation(ctx, r.Marker)
		if err != nil {
			return out, fmt.Errorf("recover source %s: %w", source, err)
		}
		recovered[source] = id
	}
	if err := c.checkInstance(ctx, target); err != nil {
		return out, err
	}
	// Nothing is written before the native decision, so a refused explicit
	// restore leaves no journal behind to block the automatic cycle.
	kept, err := c.decideNative(ctx, e, latest, target, policy)
	if err != nil {
		return out, err
	}
	if fresh {
		if err := store.WriteJournal(e, j); err != nil {
			return out, err
		}
	}
	if err := recordNative(store, e, &j, kept); err != nil {
		return out, err
	}
	latest = j.Latest()
	if deliveries, _, err = c.deliverNative(ctx, store, e, latest, target); err != nil {
		return out, err
	}
	// Scrollback replay is decided once, before any create, so a lost
	// acknowledgement can never start a second replay for the same block. Only
	// the sessions this invocation creates take part: completed and in-flight
	// sessions keep their earlier decision and are neither re-pinned nor
	// reported again.
	starts := startCommands{typed: typedStart()}
	if creating := creatingSessions(e, latest); len(creating.Snapshot.Sessions) != 0 {
		recovery = creating.Snapshot.Recovery()
		starts.replay, scrollbackSkipped, err = c.scrollbackReplay(ctx, store, creating)
		if err != nil {
			return out, err
		}
	}
	for _, session := range e.Snapshot.Sessions {
		r := latest[session.ID]
		if r.Resolved() {
			continue
		}
		var substitutions []state.CWDSubstitution
		session, substitutions, err = restoreSessionCWD(session, recordedCWD, r.Outcome == "")
		if err != nil {
			return out, err
		}
		ids := maps.Clone(r.IDMap)
		if ids == nil {
			ids = make(snapshot.IDMap)
		}
		if r.Outcome == "" {
			r.Marker, err = state.NewOperationMarker()
			if err != nil {
				return out, err
			}
			if err := store.Record(e, &j, session.ID, r.Marker, "intent", nil, substitutions...); err != nil {
				return out, err
			}
			if err := c.checkInstance(ctx, target); err != nil {
				return out, err
			}
			created, createErr := c.createSession(ctx, session, r.Marker, starts)
			if createErr != nil {
				recordErr := store.Record(e, &j, session.ID, r.Marker, "uncertain", nil)
				return out, errors.Join(fmt.Errorf("session.create outcome is uncertain; restore again to read back marker %s: %w", r.Marker, createErr), recordErr)
			}
			mappingErr := mapCreated(session, created, ids)
			maps.Copy(out.IDMap, ids)
			if err := c.checkInstance(ctx, target); err != nil {
				return out, err
			}
			if err := store.Record(e, &j, session.ID, r.Marker, "created", ids); err != nil {
				return out, errors.Join(mappingErr, err)
			}
			if mappingErr != nil {
				return out, mappingErr
			}
			r.Outcome = "created"
		} else if id, ok := recovered[session.ID]; ok {
			ids[session.ID] = id
			if err := store.Record(e, &j, session.ID, r.Marker, "recovered", ids); err != nil {
				return out, err
			}
			r.Outcome = "recovered"
		}

		view, err := c.view(ctx, ids[session.ID])
		if err != nil {
			return out, err
		}
		if r.Outcome != "configuring" && view.Label != r.Marker {
			return out, errors.New("created session no longer carries its operation marker")
		}
		if err := mapView(session, view, ids); err != nil {
			return out, err
		}
		if err := c.verifyRestored(ctx, session, ids, true); err != nil {
			return out, fmt.Errorf("created session topology: %w", err)
		}
		if r.Outcome != "configuring" {
			if err := store.Record(e, &j, session.ID, r.Marker, "configuring", ids); err != nil {
				return out, err
			}
		}
		if err := c.checkInstance(ctx, target); err != nil {
			return out, err
		}
		// The durable configuring record includes every target ID. If an
		// acknowledgement is lost here, these setters can be repeated by ID.
		if err := c.Call(ctx, "session.set_label", map[string]any{"session_id": ids[session.ID], "label": session.Label}, nil); err != nil {
			return out, err
		}
		if err := c.configureSession(ctx, session, ids); err != nil {
			return out, err
		}
		if err := c.verifyRestored(ctx, session, ids, false); err != nil {
			return out, err
		}
		if err := c.rebindReceipts(ctx, store, target.HostID, session, ids); err != nil {
			return out, err
		}
		if err := c.checkInstance(ctx, target); err != nil {
			return out, err
		}
		if err := store.Record(e, &j, session.ID, r.Marker, "complete", ids); err != nil {
			return out, err
		}
	}
	return out, nil
}

// verifyRecorded checks every session an earlier run finished against the
// live server before any unfinished work resumes. A completed session must
// still match its recorded IDs. A session Rex restored itself must still be
// live, and an explicit restore holds it to the selected generation whether
// or not an earlier cycle recorded it.
func (c Client) verifyRecorded(ctx context.Context, e state.Envelope, latest map[snapshot.SourceID]state.JournalRecord, recordedCWD map[snapshot.SourceID]string, policy nativePolicy) error {
	var live []snapshot.LiveID
	for _, session := range e.Snapshot.Sessions {
		r := latest[session.ID]
		if r.Outcome == "native" && policy == acceptLiveState {
			if live == nil {
				var err error
				if live, err = c.List(ctx); err != nil {
					return err
				}
			}
			if !slices.Contains(live, snapshot.LiveID(session.ID)) {
				return fmt.Errorf("recorded native session %s is missing", session.ID)
			}
			continue
		}
		if r.Outcome != "complete" && r.Outcome != "configuring" && r.Outcome != "native" {
			continue
		}
		session, _, err := restoreSessionCWD(session, recordedCWD, false)
		if err != nil {
			return err
		}
		if err := c.verifyRestored(ctx, session, r.IDMap, r.Outcome == "configuring"); err != nil {
			id := r.IDMap[session.ID]
			listed, listErr := c.List(ctx)
			if listErr == nil && !slices.Contains(listed, id) {
				return fmt.Errorf("recorded %s session %s is missing: %w", r.Outcome, id, err)
			}
			if r.Outcome == "native" {
				return nativeMismatch(e, session.ID, err, "save to record the live layout")
			}
			return fmt.Errorf("recorded %s session %s could not be verified: %w", r.Outcome, id, err)
		}
	}
	return nil
}

// nativeKeep is a saved session Rex brought back itself; the restore records
// it instead of creating a copy.
type nativeKeep struct {
	session snapshot.SourceID
	ids     snapshot.IDMap
}

// decideNative finds every pending saved session that Rex's own snapshot has
// already brought back under its saved ID, waits until Rex has finished with
// it, and compares it with the generation under policy. It writes nothing, so
// an explicit restore is refused before any journal changes. The waits may
// have outlived the server, so the instance is checked once after them: the
// same IDs on a new instance belong to that instance's own restore.
func (c Client) decideNative(ctx context.Context, e state.Envelope, latest map[snapshot.SourceID]state.JournalRecord, target serverIdentity, policy nativePolicy) ([]nativeKeep, error) {
	pending := creatingSessions(e, latest).Snapshot.Sessions
	if len(pending) == 0 {
		return nil, nil
	}
	live, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	var kept []nativeKeep
	for _, session := range pending {
		if !slices.Contains(live, snapshot.LiveID(session.ID)) {
			continue
		}
		ids, observed, err := c.awaitNative(ctx, session)
		if err != nil {
			return nil, err
		}
		if policy == matchGeneration {
			if err := snapshot.Compare(snapshot.Single(session), observed, ids); err != nil {
				return nil, nativeMismatch(e, session.ID, err, "save to keep the live layout, or destroy the session to restore the saved one")
			}
		}
		kept = append(kept, nativeKeep{session: session.ID, ids: ids})
	}
	if len(kept) == 0 {
		return nil, nil
	}
	if err := c.checkInstance(ctx, target); err != nil {
		return nil, err
	}
	return kept, nil
}

// recordNative records each kept session with outcome native. Rex keeps
// window, layer, split and block IDs as well, so the record maps every saved
// ID that is live to itself; nothing is created, configured, or replayed.
func recordNative(store *state.Store, e state.Envelope, j *state.Journal, kept []nativeKeep) error {
	for _, k := range kept {
		marker, err := state.NewOperationMarker()
		if err != nil {
			return err
		}
		if err := store.Record(e, j, k.session, marker, "native", k.ids); err != nil {
			return err
		}
	}
	return nil
}

// nativeMismatch refuses an explicit restore over a session Rex restored
// that differs from the generation. The advice depends on whether the
// session is already recorded: a native record is terminal, so destroying
// the session afterwards would leave it missing rather than recreate it.
func nativeMismatch(e state.Envelope, session snapshot.SourceID, err error, advice string) error {
	return fmt.Errorf("session %s is live as Rex restored it but differs from generation %d: %w; %s", session, e.Generation, err, advice)
}

const (
	nativeReadyWait = 30 * time.Second
	nativeReadyPoll = 250 * time.Millisecond
	nativeSettle    = 2 * time.Second
)

// awaitNative waits until Rex has finished bringing session back. Rex creates
// restored blocks in the background after the server answers, and a session
// recorded before that would be saved without them and lose their receipts.
// Rex's snapshot is newer than the saved generation and may hold more or
// fewer resources, so the saved inventory cannot say when Rex is done: the
// session is ready once every pane reports its working directory and nothing
// in the session has changed for nativeSettle. It returns the saved IDs that
// are live and the capture they came from, so the map and any comparison
// agree.
func (c Client) awaitNative(ctx context.Context, session snapshot.Session) (snapshot.IDMap, snapshot.Snapshot, error) {
	inventory, err := snapshot.Single(session).Inventory()
	if err != nil {
		return nil, snapshot.Snapshot{}, err
	}
	deadline := time.Now().Add(nativeReadyWait)
	var previous map[snapshot.SourceID]string
	settled := time.Now()
	for {
		observed, report, err := c.Capture(ctx, []snapshot.LiveID{snapshot.LiveID(session.ID)})
		if err != nil {
			return nil, snapshot.Snapshot{}, err
		}
		live, err := observed.Inventory()
		if err != nil {
			return nil, snapshot.Snapshot{}, err
		}
		if !maps.Equal(live, previous) {
			previous, settled = live, time.Now()
		}
		ids := make(snapshot.IDMap, len(inventory))
		for id, kind := range inventory {
			if live[id] == kind {
				ids[id] = snapshot.LiveID(id)
			}
		}
		// A session Rex has listed but not yet populated has no panes and a
		// stable shape; it must not settle into a record of its ID alone. A
		// saved session with no panes is populated as soon as it is live.
		populated := len(observed.Sessions) == 1 && (len(observed.Sessions[0].Blocks()) != 0 || len(inventory) == 1)
		running := populated && len(report.CWDSubstitutions) == 0
		if running && time.Since(settled) >= nativeSettle {
			return ids, observed, nil
		}
		if ctx.Err() != nil {
			return nil, snapshot.Snapshot{}, fmt.Errorf("waiting for the Rex server to restore session %s (%s): %w", session.ID, nativeProgress(running, len(ids), len(inventory)), ctx.Err())
		}
		if time.Now().After(deadline) {
			return nil, snapshot.Snapshot{}, fmt.Errorf("the Rex server is still restoring session %s (%s); retry later", session.ID, nativeProgress(running, len(ids), len(inventory)))
		}
		pause(ctx, nativeReadyPoll)
	}
}

func nativeProgress(running bool, present, saved int) string {
	if !running {
		return "its shells are still starting"
	}
	return fmt.Sprintf("%d of %d saved resources are present", present, saved)
}

// New creates resolve missing directories once, before their durable intent.
// Previously started sessions use only that recorded choice; a directory
// reappearing must not change what a lost acknowledgement is compared against.
func restoreSessionCWD(session snapshot.Session, recorded map[snapshot.SourceID]string, newCreate bool) (snapshot.Session, []state.CWDSubstitution, error) {
	var substitutions []state.CWDSubstitution
	var home string
	var node func(snapshot.Node) (snapshot.Node, error)
	node = func(n snapshot.Node) (snapshot.Node, error) {
		if n.Block != nil {
			b := *n.Block
			if cwd, ok := recorded[b.ID]; ok {
				b.CWD = cwd
			} else if newCreate {
				cwd, err := snapshot.CanonicalCWD(b.CWD)
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
					if home == "" {
						if home, err = snapshot.HomeCWD(); err != nil {
							return snapshot.Node{}, fmt.Errorf("block %s home cwd: %w", b.ID, err)
						}
					}
					substitutions = append(substitutions, state.CWDSubstitution{BlockID: b.ID, SavedCWD: b.CWD, RestoredCWD: home})
					cwd, err = home, nil
				}
				if err != nil {
					return snapshot.Node{}, fmt.Errorf("block %s restore cwd: %w", b.ID, err)
				}
				b.CWD = cwd
			}
			return snapshot.Node{Block: &b}, nil
		}
		p := *n.Split
		var err error
		p.Before, err = node(p.Before)
		if err != nil {
			return snapshot.Node{}, err
		}
		p.After, err = node(p.After)
		return snapshot.Node{Split: &p}, err
	}
	session.Windows = slices.Clone(session.Windows)
	for i := range session.Windows {
		layout, err := node(session.Windows[i].Layout)
		if err != nil {
			return snapshot.Session{}, nil, err
		}
		session.Windows[i].Layout = layout
	}
	return session, substitutions, nil
}

// creatingSessions is e restricted to the sessions a restore has not yet
// attempted, the only ones whose blocks can start with a replay command.
func creatingSessions(e state.Envelope, latest map[snapshot.SourceID]state.JournalRecord) state.Envelope {
	e.Snapshot.Sessions = slices.DeleteFunc(slices.Clone(e.Snapshot.Sessions), func(session snapshot.Session) bool {
		return latest[session.ID].Outcome != ""
	})
	return e
}

func (c Client) checkInstance(ctx context.Context, want serverIdentity) error {
	got, err := c.whoami(ctx)
	if err != nil {
		return err
	}
	if got.HostID != want.HostID || got.InstanceID != want.InstanceID {
		return errors.New("the Rex server instance changed during restore")
	}
	return nil
}

func (c Client) findOperation(ctx context.Context, marker string) (snapshot.LiveID, error) {
	sessions, err := c.List(ctx)
	if err != nil {
		return "", err
	}
	var match snapshot.LiveID
	seen := make(map[snapshot.LiveID]bool)
	for _, id := range sessions {
		if id == "" || len(id) > 256 || seen[id] {
			return "", errors.New("invalid or duplicate live session ID")
		}
		seen[id] = true
		view, err := c.view(ctx, id)
		if err != nil {
			return "", err
		}
		if view.Label == marker {
			if match != "" {
				return "", fmt.Errorf("ambiguous operation marker %s: multiple live sessions", marker)
			}
			match = id
		}
	}
	if match == "" {
		return "", fmt.Errorf("operation marker %s has zero live matches; creation remains uncertain", marker)
	}
	return match, nil
}

func (c Client) verifyRestored(ctx context.Context, session snapshot.Session, ids snapshot.IDMap, configuring bool) error {
	observed, report, err := c.Capture(ctx, []snapshot.LiveID{ids[session.ID]})
	if err != nil {
		return err
	}
	if len(report.CWDSubstitutions) != 0 {
		return fmt.Errorf("restored block %s still has no observable cwd", report.CWDSubstitutions[0].BlockID)
	}
	if len(report.Unsupported) != 0 || len(observed.Sessions) != 1 || len(observed.Sessions[0].Windows) != len(session.Windows) {
		return errors.New("created session contains unexpected resources")
	}
	if configuring {
		// Only the fields about to be set may differ before configuration.
		got := &observed.Sessions[0]
		got.Label, got.ActiveWindow = session.Label, snapshot.SourceID(ids[session.ActiveWindow])
		for i, w := range session.Windows {
			got.Windows[i].Focus = snapshot.SourceID(ids[w.Focus])
			got.Windows[i].Zoom = snapshot.SourceID(ids[w.Zoom])
		}
	}
	return snapshot.Compare(snapshot.Single(session), observed, ids)
}
