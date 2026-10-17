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
	return c.restoreSaved(ctx, store, e, target)
}

func (c Client) restoreSaved(ctx context.Context, store *state.Store, e state.Envelope, target serverIdentity) (out state.JournalStatus, err error) {
	if target.HostID != e.HostID {
		return out, errors.New("state belongs to a different Rex host")
	}
	j, err := store.LoadJournal(e, target.HostID, target.InstanceID)
	if errors.Is(err, state.ErrNoJournal) {
		j = state.NewJournal(e, target.HostID, target.InstanceID)
		err = store.WriteJournal(e, j)
	}
	if err != nil {
		return out, err
	}
	out = j.Status(e)
	defer func() {
		acknowledged := out.IDMap
		out = j.Status(e)
		maps.Copy(out.IDMap, acknowledged)
	}()

	latest := j.Latest()
	recordedCWD := make(map[snapshot.SourceID]string)
	for _, sub := range out.CWDSubstitutions {
		recordedCWD[sub.BlockID] = sub.RestoredCWD
	}
	// Completed sessions are historical creations, never candidates for a new
	// create. Verify their recorded IDs before resuming any unfinished work.
	for _, session := range e.Snapshot.Sessions {
		r := latest[session.ID]
		if r.Outcome != "complete" && r.Outcome != "configuring" {
			continue
		}
		session, _, err = restoreSessionCWD(session, recordedCWD, false)
		if err != nil {
			return out, err
		}
		if err := c.verifyRestored(ctx, session, r.IDMap, r.Outcome == "configuring"); err != nil {
			id := r.IDMap[session.ID]
			live, listErr := c.List(ctx)
			if listErr == nil && !slices.Contains(live, id) {
				return out, fmt.Errorf("recorded %s session %s is missing: %w", r.Outcome, id, err)
			}
			return out, fmt.Errorf("recorded %s session %s could not be verified: %w", r.Outcome, id, err)
		}
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
	for _, session := range e.Snapshot.Sessions {
		r := latest[session.ID]
		if r.Outcome == "complete" {
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
			created, createErr := c.createSession(ctx, session, r.Marker)
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

func (c Client) checkInstance(ctx context.Context, want serverIdentity) error {
	got, err := c.identity(ctx)
	if err != nil {
		return err
	}
	if got.HostID != want.HostID || got.InstanceID != want.InstanceID {
		return errors.New("Rex server instance changed during restore")
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
	return snapshot.Compare(snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{session}}, observed, ids)
}
