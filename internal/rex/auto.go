package rex

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/blacktop/rexurrect/internal/rexbin"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

const (
	DefaultAutoWait    = 2 * time.Minute
	DefaultAutoTimeout = 5 * time.Minute
	autoPollInterval   = 2 * time.Second
)

type AutoResult struct {
	RexBinarySubstitution    *rexbin.Substitution       `json:"rex_binary_substitution,omitempty"`
	State                    string                     `json:"state"`
	Generation               uint64                     `json:"generation"`
	HostID                   string                     `json:"host_id"`
	InstanceID               string                     `json:"instance_id"`
	RexAPI                   RexAPI                     `json:"rex_api,omitzero"`
	RestoreOutcome           string                     `json:"restore_outcome"`
	Restored                 snapshot.Recovery          `json:"restored,omitzero"`
	Native                   int                        `json:"native,omitempty"`
	SavedGeneration          uint64                     `json:"saved_generation,omitempty"`
	Reason                   string                     `json:"reason,omitempty"`
	CWDSubstitutions         []state.CWDSubstitution    `json:"cwd_substitutions,omitempty"`
	NotResumed               []state.AgentNotResumed    `json:"not_resumed,omitempty"`
	ScrollbackSkipped        []snapshot.Skip            `json:"scrollback_skipped,omitempty"`
	PrunedReceipts           []snapshot.SourceID        `json:"pruned_receipts,omitempty"`
	CaptureCWDSubstitutions  []snapshot.CWDSubstitution `json:"capture_cwd_substitutions,omitempty"`
	CaptureScrollbackSkipped []snapshot.Skip            `json:"capture_scrollback_skipped,omitempty"`
	CaptureForegroundSkipped []snapshot.Skip            `json:"capture_foreground_skipped,omitempty"`
}

// AutoCycle holds the same nonblocking lock as manual save and restore for its
// entire decision, recovery, and publication. An unchanged capture also succeeds.
func (c Client) AutoCycle(ctx context.Context, dir string, wait, timeout time.Duration) (out AutoResult, err error) {
	out.State, out.RestoreOutcome = "unreadable", "not_checked"
	defer func() {
		if err != nil {
			out.Reason = err.Error()
		}
	}()
	if wait <= 0 || timeout <= 0 {
		out.State = "invalid_options"
		return out, errors.New("--wait and --timeout must be positive durations")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return out, err
	}
	c.Binary, out.RexBinarySubstitution, err = rexbin.Resolve(c.Binary)
	if err != nil {
		out.State = "unavailable"
		return out, err
	}
	store, err := state.Open(dir)
	if err != nil {
		if errors.Is(err, state.ErrLocked) {
			out.State = "locked"
		}
		return out, err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	current, err := store.LoadCurrent()
	if err != nil && !errors.Is(err, state.ErrNoCurrent) {
		return out, err
	}
	out.Generation = current.Envelope.Generation
	target, err := c.waitAvailable(ctx, wait)
	if err != nil {
		out.State = "unavailable"
		return out, err
	}
	out.HostID, out.InstanceID = target.HostID, target.InstanceID
	out.RestoreOutcome = "not_started"
	if out.Generation == 0 {
		out.State = "first_run"
	} else {
		if current.Envelope.HostID != target.HostID {
			out.State = "other_host"
			return out, errors.New("state belongs to a different Rex host")
		}
		sameInstance := current.Envelope.InstanceID == target.InstanceID
		status, err := c.status(ctx, store, current, target)
		if err != nil {
			return out, err
		}
		out.RestoreOutcome = status.Journal.Outcome
		out.State = "new_instance_unresolved"
		if sameInstance {
			out.State = "same_instance_blocked"
		}
		// On a new instance, restore resolves outstanding creates by marker
		// read-back before it can create any missing sessions.
		resumable := !sameInstance && (out.RestoreOutcome == "pending" || out.RestoreOutcome == "uncertain")
		if !resumable && out.RestoreOutcome != "not_started" && out.RestoreOutcome != "complete" {
			return out, fmt.Errorf("current generation %d has unresolved restore outcome %s", out.Generation, out.RestoreOutcome)
		}
		if sameInstance {
			out.State = "same_instance"
		} else {
			restored, restoreErr := c.restoreSaved(ctx, store, current.Envelope, target, acceptLiveState)
			out.CWDSubstitutions = restored.CWDSubstitutions
			out.NotResumed = restored.NotResumed
			out.ScrollbackSkipped = restored.ScrollbackSkipped
			out.Restored = restored.Recovery
			out.Native = restored.Native
			if restored.Outcome != "" {
				out.RestoreOutcome = restored.Outcome
			}
			if restoreErr != nil {
				return out, restoreErr
			}
			if out.RestoreOutcome != "complete" {
				return out, fmt.Errorf("restore ended with outcome %s", out.RestoreOutcome)
			}
			out.State = "new_instance"
		}
	}
	saved, captured, err := c.save(ctx, store, false, &target, &current)
	// The capture read the server identity; its advertised API range is
	// reported even when publication then failed.
	out.RexAPI = captured.API
	if err != nil {
		return out, err
	}
	out.SavedGeneration = saved.Generation
	out.PrunedReceipts = saved.Report.PrunedReceipts
	out.CaptureCWDSubstitutions = saved.Report.CWDSubstitutions
	out.CaptureScrollbackSkipped = saved.Report.ScrollbackSkipped
	out.CaptureForegroundSkipped = saved.Report.ForegroundSkipped
	if saved.Generation == 0 {
		out.Reason = "unchanged"
	}
	return out, nil
}

// Availability is established only by whoami on the explicit, non-autostarting
// client. The wait deadline is nested inside the single cycle deadline.
func (c Client) waitAvailable(ctx context.Context, wait time.Duration) (serverIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var last error
	for ctx.Err() == nil {
		id, err := c.whoami(ctx)
		if err == nil {
			return id, nil
		}
		last = err
		pause(ctx, autoPollInterval)
	}
	return serverIdentity{}, fmt.Errorf("no reachable Rex server: %w", errors.Join(ctx.Err(), last))
}
