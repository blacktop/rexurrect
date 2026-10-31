package rex

import (
	"context"
	"fmt"

	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// deliveryShells are the shells whose rexurrect hook can check their own
// line buffer and take the saved input: fish runs a receipt's resume
// command, zsh can only type it and leave Enter to the user.
var deliveryShells = map[string]bool{"fish": true, "zsh": true}

// knownShells are programs that count as a shell waiting at its prompt
// rather than a command the user is running.
var knownShells = map[string]bool{"fish": true, "zsh": true, "bash": true, "sh": true, "dash": true, "ksh": true, "tcsh": true, "csh": true, "nu": true, "elvish": true, "xonsh": true, "pwsh": true}

// deliverNative records, for every block of a natively restored session that
// has a receipt or a recorded foreground command, what happened to that
// input. It works from the journal's native records rather than from what
// this call kept, so a run interrupted after its native records still
// finishes the deliveries on a retry. Rex restores such a pane as a bare
// shell, so the input has to be handed to that shell, and only a hook inside
// the shell can check that its line buffer is empty before taking it. Each
// block gets one terminal record per server instance, so a later cycle never
// types the same command twice; an intent left by an interrupted run is
// closed as uncertain, never resent. changed reports whether anything was
// recorded.
func (c Client) deliverNative(ctx context.Context, store *state.Store, e state.Envelope, latest map[snapshot.SourceID]state.JournalRecord, target serverIdentity) (outcomes []state.Delivery, changed bool, err error) {
	log, err := store.LoadDeliveries(target.HostID, target.InstanceID)
	if err != nil {
		return nil, false, err
	}
	recorded := log.Latest()
	for _, session := range e.Snapshot.Sessions {
		r := latest[session.ID]
		if r.Outcome != "native" {
			continue
		}
		for _, b := range session.Blocks() {
			d, ok := state.DeliveryFor(e, session.ID, *b)
			previous := recorded[b.ID]
			if !ok || r.IDMap[b.ID] == "" || previous.Terminal() {
				continue
			}
			if previous.Outcome == state.DeliveryIntent {
				err = store.ResolveDelivery(&log, b.ID, state.DeliveryUncertain, "interrupted before the shell acknowledged the input")
			} else {
				d.Shell, d.Outcome, d.Reason = c.paneGate(ctx, r.IDMap[session.ID], r.IDMap[b.ID])
				err = store.RecordDelivery(e, &log, d)
			}
			if err != nil {
				return nil, false, err
			}
			changed = true
		}
	}
	return log.Outcomes(), changed, nil
}

type paneProcess struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
}

// paneGate reads the pane's process tree and decides whether its shell could
// take input: the foreground process must be a shell that either is the
// pane's child or hangs from it through a complete chain of login processes.
// Rex leaves the chain out when it is incomplete, and a shell with an
// unknown parent could be running under anything, so that pane is skipped as
// unknown. It reads only; the shell's own hook is what checks the line
// buffer and performs the write, and until that hook is installed every
// supported shell is skipped with that reason.
func (c Client) paneGate(ctx context.Context, session, block snapshot.LiveID) (shell, outcome, reason string) {
	var process struct {
		Foreground *paneProcess  `json:"foreground"`
		Child      *paneProcess  `json:"child"`
		Ancestors  []paneProcess `json:"ancestors"`
	}
	args := map[string]any{"session_id": session, "block_id": block, "args": map[string]any{}}
	if err := c.Call(ctx, "com.superlogical.terminal.process", args, &process); err != nil {
		return "", state.DeliverySkipped, "pane state unknown: " + bounded(err.Error(), 200)
	}
	if process.Foreground == nil || process.Child == nil {
		return "", state.DeliverySkipped, "pane state unknown: no foreground or child process"
	}
	name := process.Foreground.Name
	if !knownShells[name] {
		return "", state.DeliverySkipped, fmt.Sprintf("busy: %s is running", name)
	}
	if process.Foreground.PID != process.Child.PID {
		for _, a := range process.Ancestors {
			if a.Name != "login" {
				return name, state.DeliverySkipped, fmt.Sprintf("busy: %s runs under %s", name, a.Name)
			}
		}
		if n := len(process.Ancestors); n == 0 || process.Ancestors[n-1].PID != process.Child.PID {
			return name, state.DeliverySkipped, "pane state unknown: process chain to the pane's child is incomplete"
		}
	}
	if !deliveryShells[name] {
		return name, state.DeliverySkipped, fmt.Sprintf("shell %s is not supported; fish runs commands, zsh types them", name)
	}
	return name, state.DeliverySkipped, "no delivery hook in this shell; install it with rexurrect hook " + name
}
