package rex

import (
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// attachForeground copies each live shell block's recorded foreground command
// into the capture and prunes the records of blocks that are gone. A record
// that cannot be used, or a records directory that is not private, is a
// per-block skip; it never fails the save.
func attachForeground(store *state.Store, e *state.Envelope) error {
	live := make(map[snapshot.SourceID]bool)
	blocks := make(map[snapshot.SourceID]*snapshot.Block)
	var ids []snapshot.SourceID
	for _, session := range e.Snapshot.Sessions {
		for _, b := range session.Blocks() {
			live[b.ID], blocks[b.ID] = true, b
			ids = append(ids, b.ID)
		}
	}
	for _, unsupported := range e.Report.Unsupported {
		for _, id := range unsupported.BlockIDs {
			live[id] = true
		}
	}
	records, skips, err := store.ForegroundRecords(ids)
	if err != nil {
		for _, id := range ids {
			e.Report.ForegroundSkipped = append(e.Report.ForegroundSkipped, snapshot.Skip{BlockID: id, Reason: "foreground records: " + bounded(err.Error(), 200)})
		}
		return nil
	}
	for id, command := range records {
		blocks[id].Foreground = command
	}
	e.Report.ForegroundSkipped = append(e.Report.ForegroundSkipped, skips...)
	_, err = store.PruneForeground(live)
	return err
}
