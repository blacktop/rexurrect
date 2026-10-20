package rex

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// DefaultLockTimeout bounds how long receipt commands wait for the state lock
// when Client.LockTimeout is zero.
const DefaultLockTimeout = 30 * time.Second

// MaxLockTimeout caps Client.LockTimeout.
const MaxLockTimeout = 60 * time.Second

// ValidateLockTimeout checks an explicit receipt lock wait.
func ValidateLockTimeout(timeout time.Duration) error {
	if timeout <= 0 || timeout > MaxLockTimeout {
		return errors.New("--lock-timeout must be positive and at most 60s")
	}
	return nil
}

func (c Client) withReceipts(ctx context.Context, dir string, use func(context.Context, *state.Store, serverIdentity) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	lockTimeout := c.LockTimeout
	if lockTimeout == 0 {
		lockTimeout = DefaultLockTimeout
	}
	if err := ValidateLockTimeout(lockTimeout); err != nil {
		return err
	}
	store, err := state.OpenWait(ctx, dir, lockTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	id, err := c.identity(ctx)
	if err != nil {
		return err
	}
	current, err := store.LoadCurrent()
	if err != nil && !errors.Is(err, state.ErrNoCurrent) {
		return err
	}
	if err == nil && current.Envelope.HostID != id.HostID {
		return errors.New("state belongs to a different Rex host")
	}
	return use(ctx, store, id)
}

// SetReceipt is the sole registration path for the CLI and integrations. A
// shell block must be live on this server; process metadata is never consulted.
func (c Client) SetReceipt(ctx context.Context, dir string, r receipt.Receipt) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return c.withReceipts(ctx, dir, func(ctx context.Context, store *state.Store, id serverIdentity) error {
		var listed struct {
			Blocks []listedBlock `json:"blocks"`
		}
		if err := c.Call(ctx, "session.list_blocks", map[string]any{"session_id": r.SessionID}, &listed); err != nil {
			return err
		}
		if len(listed.Blocks) > 16384 {
			return errors.New("too many registered blocks")
		}
		matches := 0
		for _, block := range listed.Blocks {
			if string(block.ID) == r.BlockID && block.Flavor == shellFlavor {
				matches++
			}
		}
		if matches != 1 {
			return errors.New("receipt block is not a live Rex terminal in the selected session")
		}
		if err := c.checkInstance(ctx, id); err != nil {
			return err
		}
		return store.SetReceipt(id.HostID, r)
	})
}

// ClearReceipt removes the receipt under key when every supplied guard matches.
func (c Client) ClearReceipt(
	ctx context.Context, dir string, key receipt.Key, guard receipt.ClearGuard,
) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if err := guard.Validate(); err != nil {
		return err
	}
	return c.withReceipts(ctx, dir, func(_ context.Context, store *state.Store, id serverIdentity) error {
		return store.ClearReceipt(id.HostID, key, guard)
	})
}

func (c Client) ListReceipts(ctx context.Context, dir string) (out []receipt.Receipt, err error) {
	err = c.withReceipts(ctx, dir, func(_ context.Context, store *state.Store, id serverIdentity) error {
		var err error
		out, err = store.LoadReceipts(id.HostID)
		return err
	})
	return out, err
}

func attachReceipts(store *state.Store, e *state.Envelope) ([]receipt.Receipt, error) {
	entries, err := store.LoadReceipts(e.HostID)
	if err != nil {
		return nil, err
	}
	live := make(map[receipt.Key]bool)
	blocks := make(map[receipt.Key]*snapshot.Block)
	for _, session := range e.Snapshot.Sessions {
		for _, b := range session.Blocks() {
			key := receipt.Key{SessionID: string(session.ID), BlockID: string(b.ID)}
			live[key], blocks[key] = true, b
		}
	}
	for _, unsupported := range e.Report.Unsupported {
		for _, block := range unsupported.BlockIDs {
			live[receipt.Key{SessionID: string(unsupported.SessionID), BlockID: string(block)}] = true
		}
	}
	kept := make([]receipt.Receipt, 0, len(entries))
	for _, r := range entries {
		if !live[r.Key] {
			e.Report.PrunedReceipts = append(e.Report.PrunedReceipts, snapshot.SourceID(r.BlockID))
			continue
		}
		kept = append(kept, r)
		if b := blocks[r.Key]; b != nil {
			b.AgentResume = &r
		}
	}
	slices.Sort(e.Report.PrunedReceipts)
	return kept, nil
}

// Copy opt-in receipts to the acknowledged IDs before completing the journal.
// This keeps the automatic cycle's immediate save (and a later restart) from
// losing them while hooks are excluded by the state lock. Never overwrite a
// receipt already registered for a destination block.
func (c Client) rebindReceipts(ctx context.Context, store *state.Store, host string, session snapshot.Session, ids snapshot.IDMap) error {
	var moved []receipt.Receipt
	old := make(map[receipt.Key]bool)
	for _, b := range session.Blocks() {
		if b.AgentResume == nil || b.AgentResume.Validate() != nil {
			continue
		}
		r := *b.AgentResume
		old[r.Key] = true
		r.SessionID, r.BlockID = string(ids[session.ID]), string(ids[b.ID])
		if err := r.Validate(); err != nil {
			return fmt.Errorf("mapped receipt: %w", err)
		}
		moved = append(moved, r)
	}
	if len(moved) == 0 {
		return nil
	}
	live, err := c.List(ctx)
	if err != nil {
		return err
	}
	sourceExists := slices.Contains(live, snapshot.LiveID(session.ID))
	entries, err := store.LoadReceipts(host)
	if err != nil {
		return err
	}
	kept := make([]receipt.Receipt, 0, len(entries))
	seen := make(map[receipt.Key]bool)
	for _, r := range entries {
		if sourceExists || !old[r.Key] {
			kept = append(kept, r)
			seen[r.Key] = true
		}
	}
	for _, r := range moved {
		if !seen[r.Key] {
			kept = append(kept, r)
		}
	}
	return store.WriteReceipts(host, kept)
}
