package rex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

const captureAttempts = 3

type serverIdentity struct {
	RexVersion string `json:"rex_version"`
	Commit     string `json:"commit"`
	HostID     string `json:"host_id"`
	InstanceID string `json:"instance_id"`
}

func (c Client) whoami(ctx context.Context) (serverIdentity, error) {
	var id serverIdentity
	data, err := c.command(ctx, "whoami", "--json")
	if err != nil {
		return id, err
	}
	if err := json.Unmarshal(data, &id); err != nil {
		return id, fmt.Errorf("decode Rex identity: %w", err)
	}
	for _, field := range []string{id.HostID, id.InstanceID, id.RexVersion} {
		if field == "" || len(field) > 256 {
			return id, errors.New("invalid Rex server identity")
		}
	}
	return id, nil
}

func (c Client) identity(ctx context.Context) (serverIdentity, error) {
	id, err := c.whoami(ctx)
	if err != nil {
		return id, err
	}
	var version struct {
		RexVersion string `json:"rex_version"`
		Commit     string `json:"commit"`
	}
	if err := c.Call(ctx, "server.version", map[string]any{}, &version); err != nil {
		return id, err
	}
	if id.RexVersion != version.RexVersion {
		return id, errors.New("Rex server version changed during identity read")
	}
	id.Commit = version.Commit
	if id.Commit == "" || len(id.Commit) > 256 {
		return id, errors.New("invalid Rex server identity")
	}
	return id, nil
}

type captureBoundary struct {
	identity  serverIdentity
	sessions  []snapshot.LiveID
	revisions []uint64
}

func (c Client) boundary(ctx context.Context) (captureBoundary, error) {
	var b captureBoundary
	var err error
	if b.identity, err = c.identity(ctx); err != nil {
		return b, err
	}
	if b.sessions, err = c.List(ctx); err != nil {
		return b, err
	}
	slices.Sort(b.sessions)
	for i, id := range b.sessions {
		if id == "" || len(id) > 256 || i > 0 && b.sessions[i-1] == id {
			return b, errors.New("invalid or duplicate session identity")
		}
		var view struct {
			ID       snapshot.LiveID `json:"session_id"`
			Revision *uint64         `json:"revision"`
		}
		if err := c.Call(ctx, "session.view", map[string]any{"session_id": id}, &view); err != nil {
			return b, err
		}
		if view.ID != id || view.Revision == nil {
			return b, fmt.Errorf("session %s has no valid structural revision", id)
		}
		b.revisions = append(b.revisions, *view.Revision)
	}
	return b, nil
}

func (c Client) stableCapture(ctx context.Context) (state.Envelope, error) {
	for range captureAttempts {
		before, err := c.boundary(ctx)
		if err != nil {
			return state.Envelope{}, fmt.Errorf("before capture: %w", err)
		}
		saved, report, captureErr := c.Capture(ctx, before.sessions)
		after, err := c.boundary(ctx)
		if err != nil {
			return state.Envelope{}, fmt.Errorf("after capture: %w", err)
		}
		if before.identity != after.identity || !slices.Equal(before.sessions, after.sessions) || !slices.Equal(before.revisions, after.revisions) {
			continue
		}
		if captureErr != nil {
			return state.Envelope{}, captureErr
		}
		return state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: before.identity.RexVersion, Commit: before.identity.Commit, HostID: before.identity.HostID, InstanceID: before.identity.InstanceID, Snapshot: saved, Report: &report}, nil
	}
	return state.Envelope{}, fmt.Errorf("unstable Rex capture after %d attempts: server identity, session set, or structural revision changed", captureAttempts)
}

// Save captures every live session without mutating Rex. The state lock covers
// capture, validation, the empty-server guard, and durable publication.
func (c Client) Save(ctx context.Context, dir string, allowEmpty bool) (saved state.Envelope, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	store, err := state.Open(dir)
	if err != nil {
		return saved, err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	return c.save(ctx, store, allowEmpty, nil, nil)
}

// expected binds an automatic save to the instance whose recovery was checked.
func (c Client) save(ctx context.Context, store *state.Store, allowEmpty bool, expected *serverIdentity, current *state.Current) (state.Envelope, error) {
	captured, err := c.stableCapture(ctx)
	if err != nil {
		return state.Envelope{}, err
	}
	if expected != nil && (captured.HostID != expected.HostID || captured.InstanceID != expected.InstanceID) {
		return state.Envelope{}, errors.New("Rex server instance changed during automatic cycle")
	}
	if err := ctx.Err(); err != nil {
		return state.Envelope{}, err
	}
	kept, err := attachReceipts(store, &captured)
	if err != nil {
		return state.Envelope{}, err
	}
	if current != nil {
		previous := current.Envelope
		// Preserve the empty-server guard, including an explicitly saved empty
		// current generation whose last-known-good is still nonempty.
		emptyAllowed := len(captured.Snapshot.Sessions) != 0 || current.LastKnownGood == nil || len(current.LastKnownGood.Snapshot.Sessions) == 0
		if previous.Generation != 0 && emptyAllowed && previous.HostID == captured.HostID && previous.InstanceID == captured.InstanceID && previous.RexVersion == captured.RexVersion && previous.Commit == captured.Commit && reflect.DeepEqual(previous.Snapshot, captured.Snapshot) && reflect.DeepEqual(previous.Report, captured.Report) {
			// A prior publication may have succeeded before receipt pruning
			// failed. Its matching durable report permits finishing that step.
			if len(captured.Report.PrunedReceipts) != 0 {
				if err := store.WriteReceipts(captured.HostID, kept); err != nil {
					return state.Envelope{}, err
				}
			}
			return captured, nil // Generation zero means no publication.
		}
	}
	saved, err := store.Publish(captured, allowEmpty)
	if err == nil && len(captured.Report.PrunedReceipts) != 0 {
		err = store.WriteReceipts(captured.HostID, kept)
	}
	return saved, err
}

type Status struct {
	Server        string              `json:"server"`
	Generation    uint64              `json:"generation"`
	LastKnownGood uint64              `json:"last_known_good"`
	CapturedAt    time.Time           `json:"captured_at"`
	RexVersion    string              `json:"rex_version"`
	Commit        string              `json:"commit"`
	HostID        string              `json:"host_id"`
	InstanceID    string              `json:"instance_id"`
	Sessions      int                 `json:"sessions"`
	Windows       int                 `json:"windows"`
	Blocks        int                 `json:"blocks"`
	Unsupported   int                 `json:"unsupported"`
	Journal       state.JournalStatus `json:"journal"`
}

// Status reports the validated saved generation. A reachable endpoint with the
// same host identity is required; the saved instance may predate a restart.
func (c Client) Status(ctx context.Context, dir string) (out Status, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	store, err := state.Open(dir)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	current, err := store.LoadCurrent()
	if err != nil {
		return out, err
	}
	id, err := c.identity(ctx)
	if err != nil {
		return out, err
	}
	return c.status(ctx, store, current, id)
}

func (c Client) status(ctx context.Context, store *state.Store, current state.Current, id serverIdentity) (out Status, err error) {
	e := current.Envelope
	if id.HostID != e.HostID {
		return out, errors.New("state belongs to a different Rex host")
	}
	out = Status{Server: "unix://" + c.Socket, Generation: e.Generation, CapturedAt: e.CapturedAt, RexVersion: e.RexVersion, Commit: e.Commit, HostID: e.HostID, InstanceID: e.InstanceID, Unsupported: len(e.Report.Unsupported)}
	if current.LastKnownGood != nil {
		out.LastKnownGood = current.LastKnownGood.Generation
	}
	ids, err := e.Snapshot.Inventory()
	if err != nil {
		return out, err
	}
	for _, kind := range ids {
		switch kind {
		case "session":
			out.Sessions++
		case "window":
			out.Windows++
		case "block":
			out.Blocks++
		}
	}
	j, err := store.LoadJournal(e, id.HostID, id.InstanceID)
	if errors.Is(err, state.ErrNoJournal) {
		out.Journal = state.JournalStatus{Generation: e.Generation, HostID: id.HostID, InstanceID: id.InstanceID, Outcome: "not_started", IDMap: make(snapshot.IDMap)}
	} else if err != nil {
		return out, err
	} else {
		out.Journal = j.Status(e)
		latest := j.Latest()
		var completed []snapshot.LiveID
		for _, session := range e.Snapshot.Sessions {
			if r := latest[session.ID]; r.Outcome == "complete" {
				completed = append(completed, r.IDMap[session.ID])
			}
		}
		if len(completed) != 0 {
			live, err := c.List(ctx)
			if err != nil {
				return out, err
			}
			for _, id := range completed {
				if !slices.Contains(live, id) {
					out.Journal.MissingSessions = append(out.Journal.MissingSessions, id)
				}
			}
			if len(out.Journal.MissingSessions) != 0 && out.Journal.Outcome == "complete" {
				out.Journal.Outcome = "complete_missing"
			}
		}
	}
	return out, nil
}
