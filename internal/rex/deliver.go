package rex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

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

const (
	deliveryAckWait  = 2 * time.Second
	deliveryAckPoll  = 50 * time.Millisecond
	deliveryHookWait = 3 * time.Second
	deliveryHookPoll = 100 * time.Millisecond
)

// deliverNative hands, for every block of a natively restored session that
// has a receipt or a recorded foreground command, that input to the shell in
// the pane, or records why it did not. It works from the journal's native
// records rather than from what this call kept, so a run interrupted after
// its native records still finishes the deliveries on a retry. Rex restores
// such a pane as a bare shell, and only the hook inside that shell can check
// that its line buffer is empty before taking the input, so rexurrect never
// types: it asks the hook and records the answer. Each block gets one
// terminal record per server instance, so a later cycle never delivers the
// same command twice; an intent left by an interrupted run is settled from
// what its request and acknowledgement show, never resent. changed reports
// whether anything was recorded.
func (c Client) deliverNative(ctx context.Context, store *state.Store, e state.Envelope, latest map[snapshot.SourceID]state.JournalRecord, target serverIdentity) (outcomes []state.Delivery, changed bool, err error) {
	log, err := store.LoadDeliveries(target.HostID, target.InstanceID)
	if err != nil {
		return nil, false, err
	}
	run := deliveryRun{store: store, e: e, log: &log, target: target}
	if run.hooksInUse, err = store.DeliveryHooksInUse(); err != nil {
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
				outcome, reason := settleAttempt(store, previous, "interrupted before the hook claimed the request")
				err = store.ResolveDelivery(&log, b.ID, outcome, reason)
			} else {
				err = c.deliverBlock(ctx, run, d, r.IDMap[session.ID], r.IDMap[b.ID])
			}
			if err != nil {
				return nil, false, err
			}
			changed = true
		}
	}
	return log.Outcomes(), changed, nil
}

// deliveryRun is what every delivery of one deliverNative call shares: the
// store and log the records go to, the generation they are checked against,
// and the server instance they are bound to.
type deliveryRun struct {
	store  *state.Store
	e      state.Envelope
	log    *state.DeliveryLog
	target serverIdentity
	// hooksInUse says some pane's hook has a mailbox, so a pane without an
	// announcement yet is worth a bounded wait: its shell may still be
	// sourcing its config.
	hooksInUse bool
}

// awaitHook reads the pane's announcement, waiting a bounded time for a
// shell that is still starting when hooks are in use at all.
func (run deliveryRun) awaitHook(ctx context.Context, block snapshot.SourceID) (state.DeliveryHook, bool, error) {
	deadline := time.Now().Add(deliveryHookWait)
	for {
		hook, ok, err := run.store.DeliveryHook(block)
		if err != nil || ok || !run.hooksInUse || ctx.Err() != nil || time.Now().After(deadline) {
			return hook, ok, err
		}
		pause(ctx, deliveryHookPoll)
	}
}

// skip records that nothing reached the shell of d's block, and why.
func (run deliveryRun) skip(d state.Delivery, reason string) error {
	d.Outcome, d.Reason = state.DeliverySkipped, reason
	return run.store.RecordDelivery(run.e, run.log, d)
}

// deliveryRequest is one attempt: the record whose intent is durable, the
// pane it is for, and the hook it is addressed to.
type deliveryRequest struct {
	d              state.Delivery
	session, block snapshot.LiveID
	pane           paneState
	hook           state.DeliveryHook
	mode           string
}

// deliverBlock records one block's delivery: a skip when the pane or its
// hook rules it out, otherwise an intent, the request to the hook, and the
// outcome the hook acknowledged. A mailbox this pane's hook left unusable is
// that pane's skip, never a failure of the whole restore: the mailbox is the
// hook's, and the other panes' deliveries do not depend on it.
func (c Client) deliverBlock(ctx context.Context, run deliveryRun, d state.Delivery, session, block snapshot.LiveID) error {
	pane := c.paneGate(ctx, session, block)
	d.Shell = pane.shell
	if pane.reason != "" {
		return run.skip(d, pane.reason)
	}
	hook, ok, err := run.awaitHook(ctx, d.BlockID)
	if err != nil {
		return run.skip(d, "delivery mailbox unusable: "+bounded(err.Error(), 200))
	}
	if !ok {
		return run.skip(d, "no delivery hook in this shell; install it with rexurrect hook "+pane.shell)
	}
	if hook.Shell != pane.shell || hook.PID != pane.pid {
		return run.skip(d, fmt.Sprintf("delivery hook belongs to %s %d, not the pane's %s %d", hook.Shell, hook.PID, pane.shell, pane.pid))
	}
	req := deliveryRequest{d: d, session: session, block: block, pane: pane, hook: hook, mode: state.DeliveryModeType}
	if pane.shell == "fish" && d.Kind == state.DeliveryReceipt {
		req.mode = state.DeliveryModeRun
	}
	if req.d.Attempt, err = state.NewDeliveryAttempt(); err != nil {
		return err
	}
	req.d.Outcome = state.DeliveryIntent
	if err := run.store.RecordDelivery(run.e, run.log, req.d); err != nil {
		return err
	}
	outcome, reason, err := c.requestDelivery(ctx, run, req)
	if err != nil {
		return err
	}
	return run.store.ResolveDelivery(run.log, d.BlockID, outcome, reason)
}

// requestDelivery asks the hook to take the input and waits for its answer.
// Right before the request, the server instance, the pane and the hook's
// announcement are checked again, because the gate ran before the intent
// was durable. The request names the attempt and the hook it is for; the
// hook claims it by renaming it, so when the wait ends the request is either
// still there, and revoking it proves nothing happened, or gone, and only an
// acknowledgement of this attempt says what did. Only a changed server
// instance is an error; whatever the mailbox does is the attempt's outcome.
func (c Client) requestDelivery(ctx context.Context, run deliveryRun, req deliveryRequest) (outcome, reason string, err error) {
	if err := c.checkInstance(ctx, run.target); err != nil {
		return "", "", err
	}
	again := c.paneGate(ctx, req.session, req.block)
	if again.reason == "" && (again.pid != req.pane.pid || again.shell != req.pane.shell) {
		again.reason = fmt.Sprintf("the pane's shell changed from %s %d to %s %d", req.pane.shell, req.pane.pid, again.shell, again.pid)
	}
	if again.reason != "" {
		return state.DeliverySkipped, "pane changed before the request: " + again.reason, nil
	}
	if now, ok, err := run.store.DeliveryHook(req.d.BlockID); err != nil || !ok || now != req.hook {
		return state.DeliverySkipped, "the delivery hook changed before the request", nil
	}
	if err := run.store.WriteDeliveryRequest(req.d.BlockID, req.d.Attempt, req.hook.ID, req.mode, req.d.Input); err != nil {
		return state.DeliverySkipped, "could not publish the request: " + bounded(err.Error(), 200), nil
	}
	if err := c.wakeHook(run.store, req.d.BlockID, req.pane); err != nil {
		outcome, reason = settleAttempt(run.store, req.d, "delivery hook not running: "+bounded(err.Error(), 200))
		return outcome, reason, nil
	}
	deadline := time.Now().Add(deliveryAckWait)
	for {
		ack, ok, err := run.store.DeliveryAck(req.d.BlockID)
		if err != nil {
			outcome, reason = settleAttempt(run.store, req.d, "the hook did not claim the request; its acknowledgement is unreadable: "+bounded(err.Error(), 160))
			return outcome, reason, nil
		}
		if ok && ack.Attempt == req.d.Attempt {
			outcome, reason = ackOutcome(ack.Outcome, req.d.Kind)
			return outcome, reason, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			outcome, reason = settleAttempt(run.store, req.d, "the hook did not claim the request within "+deliveryAckWait.String())
			return outcome, reason, nil
		}
		pause(ctx, deliveryAckPoll)
	}
}

// settleAttempt decides an attempt whose answer did not arrive in time, or
// that an earlier run left open. Revoking the request succeeds only if the
// hook never claimed it, which makes the attempt a clean skip with the given
// reason. Otherwise an acknowledgement of this attempt decides, and without
// one, or with a mailbox that cannot be read, the attempt stays uncertain.
func settleAttempt(store *state.Store, d state.Delivery, unclaimed string) (outcome, reason string) {
	revoked, err := store.RevokeDeliveryRequest(d.BlockID)
	if err != nil {
		return state.DeliveryUncertain, "could not revoke the request: " + bounded(err.Error(), 200)
	}
	if revoked {
		return state.DeliverySkipped, unclaimed
	}
	ack, ok, err := store.DeliveryAck(d.BlockID)
	if err != nil {
		return state.DeliveryUncertain, "no unclaimed request and the acknowledgement is unreadable: " + bounded(err.Error(), 180)
	}
	if ok && ack.Attempt == d.Attempt {
		return ackOutcome(ack.Outcome, d.Kind)
	}
	return state.DeliveryUncertain, "no unclaimed request and no acknowledgement of this attempt"
}

// wakeHook wakes the hook: SIGURG to the fish process Rex reports as the
// pane's shell and the hook announced as itself, which a fish without the
// handler ignores, or a line into the fifo a zsh hook holds open. Nothing
// else is ever signalled or written.
func (c Client) wakeHook(store *state.Store, id snapshot.SourceID, pane paneState) error {
	switch pane.shell {
	case "fish":
		return syscall.Kill(pane.pid, syscall.SIGURG)
	case "zsh":
		path, ok, err := store.DeliveryFIFO(id)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the zsh hook has no fifo")
		}
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte("deliver\n"))
		return errors.Join(err, f.Close())
	default:
		return fmt.Errorf("no trigger for shell %s", pane.shell)
	}
}

// ackOutcome maps the hook's acknowledgement of an attempt to a delivery
// outcome.
func ackOutcome(ack, kind string) (outcome, reason string) {
	switch ack {
	case "delivered:run":
		if kind == state.DeliveryReceipt {
			return state.DeliveryResumed, ""
		}
	case "delivered:type":
		return state.DeliveryTypedOnly, ""
	case "skipped:pending_input":
		return state.DeliverySkipped, "the prompt already had input"
	case "rejected:other_hook":
		return state.DeliverySkipped, "the hook in the pane is not the one that announced itself"
	}
	return state.DeliveryUncertain, "unexpected acknowledgement " + bounded(ack, 64)
}

type paneProcess struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
}

// paneState is what the gate saw: the shell at the prompt and its pid when
// the pane can take input, otherwise the reason it cannot.
type paneState struct {
	shell  string
	pid    int
	reason string
}

// paneGate reads the pane's process tree and decides whether its shell could
// take input: the foreground process must be a shell that either is the
// pane's child or hangs from it through a complete chain of login processes.
// Rex leaves the chain out when it is incomplete, and a shell with an
// unknown parent could be running under anything, so that pane is skipped as
// unknown. It reads only; the shell's own hook is what checks the line
// buffer and takes the input.
func (c Client) paneGate(ctx context.Context, session, block snapshot.LiveID) paneState {
	var process struct {
		Foreground *paneProcess  `json:"foreground"`
		Child      *paneProcess  `json:"child"`
		Ancestors  []paneProcess `json:"ancestors"`
	}
	args := map[string]any{"session_id": session, "block_id": block, "args": map[string]any{}}
	if err := c.Call(ctx, "com.superlogical.terminal.process", args, &process); err != nil {
		return paneState{reason: "pane state unknown: " + bounded(err.Error(), 200)}
	}
	if process.Foreground == nil || process.Child == nil {
		return paneState{reason: "pane state unknown: no foreground or child process"}
	}
	name := process.Foreground.Name
	if !knownShells[name] {
		return paneState{reason: fmt.Sprintf("busy: %s is running", name)}
	}
	pane := paneState{shell: name, pid: process.Foreground.PID}
	if process.Foreground.PID != process.Child.PID {
		for _, a := range process.Ancestors {
			if a.Name != "login" {
				pane.reason = fmt.Sprintf("busy: %s runs under %s", name, a.Name)
				return pane
			}
		}
		if n := len(process.Ancestors); n == 0 || process.Ancestors[n-1].PID != process.Child.PID {
			pane.reason = "pane state unknown: process chain to the pane's child is incomplete"
			return pane
		}
	}
	if !deliveryShells[name] {
		pane.reason = fmt.Sprintf("shell %s is not supported; fish runs commands, zsh types them", name)
	}
	return pane
}
