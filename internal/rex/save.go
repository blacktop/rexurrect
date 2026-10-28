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
	PID        int    `json:"pid"`
	API        RexAPI `json:"rex_api"`
}

// RexAPI is the API revision range a server advertises in server.version.
// Rex leaves either field out when it is 0, and servers from before the
// fields existed leave both out, so 0 means "not advertised".
type RexAPI struct {
	ServerAPIMinRevision int `json:"server_api_min_revision"`
	ServerAPIRevision    int `json:"server_api_revision"`
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
		RexVersion     string `json:"rex_version"`
		Commit         string `json:"commit"`
		APIMinRevision int    `json:"api_min_revision"`
		APIRevision    int    `json:"api_revision"`
	}
	if err := c.Call(ctx, "server.version", map[string]any{}, &version); err != nil {
		return id, err
	}
	if id.RexVersion != version.RexVersion {
		return id, errors.New("the Rex server version changed during identity read")
	}
	id.Commit = version.Commit
	id.API = RexAPI{ServerAPIMinRevision: version.APIMinRevision, ServerAPIRevision: version.APIRevision}
	if id.Commit == "" || len(id.Commit) > 256 || id.API.ServerAPIMinRevision < 0 || id.API.ServerAPIRevision < 0 {
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

func (c Client) stableCapture(ctx context.Context) (state.Envelope, serverIdentity, error) {
	for range captureAttempts {
		before, err := c.boundary(ctx)
		if err != nil {
			return state.Envelope{}, serverIdentity{}, fmt.Errorf("before capture: %w", err)
		}
		saved, report, captureErr := c.Capture(ctx, before.sessions)
		after, err := c.boundary(ctx)
		if err != nil {
			return state.Envelope{}, serverIdentity{}, fmt.Errorf("after capture: %w", err)
		}
		if before.identity != after.identity || !slices.Equal(before.sessions, after.sessions) || !slices.Equal(before.revisions, after.revisions) {
			continue
		}
		if captureErr != nil {
			return state.Envelope{}, serverIdentity{}, captureErr
		}
		return state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: before.identity.RexVersion, Commit: before.identity.Commit, HostID: before.identity.HostID, InstanceID: before.identity.InstanceID, Snapshot: saved, Report: &report}, before.identity, nil
	}
	return state.Envelope{}, serverIdentity{}, fmt.Errorf("unstable Rex capture after %d attempts: server identity, session set, or structural revision changed", captureAttempts)
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
	saved, _, err = c.save(ctx, store, allowEmpty, nil, nil)
	return saved, err
}

// expected binds an automatic save to the instance whose recovery was checked.
func (c Client) save(ctx context.Context, store *state.Store, allowEmpty bool, expected *serverIdentity, current *state.Current) (state.Envelope, serverIdentity, error) {
	captured, id, err := c.stableCapture(ctx)
	if err != nil {
		return state.Envelope{}, id, err
	}
	if expected != nil && (captured.HostID != expected.HostID || captured.InstanceID != expected.InstanceID) {
		return state.Envelope{}, id, errors.New("the Rex server instance changed during automatic cycle")
	}
	if err := ctx.Err(); err != nil {
		return state.Envelope{}, id, err
	}
	kept, err := attachReceipts(store, &captured)
	if err != nil {
		return state.Envelope{}, id, err
	}
	live, pruneForeground := attachForeground(store, &captured)
	prune := func(retained state.Current) error {
		err := store.PruneScrollback(retained)
		if pruneForeground {
			err = errors.Join(err, store.PruneForeground(live))
		}
		return err
	}
	// The automatic cycle passes the Current it already loaded and is the only
	// caller that suppresses an unchanged publication; a manual save loads it
	// here once for the guards and the retention set.
	automatic := current != nil
	if !automatic {
		loaded, err := store.LoadCurrent()
		if err != nil && !errors.Is(err, state.ErrNoCurrent) {
			return state.Envelope{}, id, err
		}
		current = &loaded
	}
	if c.Scrollback {
		// The publication guards run first so a refused save leaves no
		// sidecar, state record, or keychain item behind.
		if err := state.PublishGuards(*current, captured, allowEmpty); err != nil {
			return state.Envelope{}, id, err
		}
		if err := c.captureScrollback(ctx, store, &captured); err != nil {
			return state.Envelope{}, id, err
		}
	}
	if automatic {
		previous := current.Envelope
		// Preserve the empty-server guard, including an explicitly saved empty
		// current generation whose last-known-good is still nonempty.
		emptyAllowed := len(captured.Snapshot.Sessions) != 0 || current.LastKnownGood == nil || len(current.LastKnownGood.Snapshot.Sessions) == 0
		if previous.Generation != 0 && emptyAllowed && previous.HostID == captured.HostID && previous.InstanceID == captured.InstanceID && previous.RexVersion == captured.RexVersion && previous.Commit == captured.Commit && reflect.DeepEqual(previous.Snapshot, captured.Snapshot) && reflect.DeepEqual(previous.Report, captured.Report) {
			// A prior publication may have succeeded before receipt or
			// scrollback pruning failed. Its matching durable report permits
			// finishing those steps.
			if len(captured.Report.PrunedReceipts) != 0 {
				if err := store.WriteReceipts(captured.HostID, kept); err != nil {
					return state.Envelope{}, id, err
				}
			}
			if err := prune(*current); err != nil {
				return state.Envelope{}, id, fmt.Errorf("pruning failed: %w", err)
			}
			return captured, id, nil // Generation zero means no publication.
		}
	}
	saved, err := store.Publish(captured, allowEmpty)
	if err == nil && len(captured.Report.PrunedReceipts) != 0 {
		err = store.WriteReceipts(captured.HostID, kept)
	}
	if err == nil {
		retained := state.Current{Envelope: saved}
		if previous := current.Envelope; previous.Generation != 0 {
			retained.LastKnownGood = &previous
		}
		// Stale records are pruned only now, after the guards inside Publish
		// accepted this capture; a refused save deletes nothing.
		if pruneErr := prune(retained); pruneErr != nil {
			err = fmt.Errorf("generation %d was published, but pruning failed: %w", saved.Generation, pruneErr)
		}
	}
	return saved, id, err
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
	RexAPI        RexAPI              `json:"rex_api"`
	Sessions      int                 `json:"sessions"`
	Windows       int                 `json:"windows"`
	Blocks        int                 `json:"blocks"`
	Unsupported   int                 `json:"unsupported"`
	Recovery      snapshot.Recovery   `json:"recovery"`
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
	out, err = c.status(ctx, store, current, id)
	if err == nil {
		// Classified here rather than in status, which the automatic cycle
		// calls every minute without reporting counts.
		out.Recovery = current.Envelope.Snapshot.Recovery()
	}
	return out, err
}

func (c Client) status(ctx context.Context, store *state.Store, current state.Current, id serverIdentity) (out Status, err error) {
	e := current.Envelope
	if id.HostID != e.HostID {
		return out, errors.New("state belongs to a different Rex host")
	}
	out = Status{Server: "unix://" + c.Socket, Generation: e.Generation, CapturedAt: e.CapturedAt, RexVersion: e.RexVersion, Commit: e.Commit, HostID: e.HostID, InstanceID: e.InstanceID, RexAPI: id.API, Unsupported: len(e.Report.Unsupported)}
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
