package rex

import (
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// attachForeground copies each live shell block's recorded foreground command
// into the capture and returns the set of live blocks, so the caller can prune
// stale records once the generation is published. A record that cannot be
// used is a per-block skip. A records directory that is not private skips
// every block and returns prune=false, so the caller leaves it alone; nothing
// here fails the save.
func attachForeground(store *state.Store, e *state.Envelope) (live map[snapshot.SourceID]bool, prune bool) {
	blocks := e.Snapshot.Blocks()
	live = make(map[snapshot.SourceID]bool, len(blocks))
	ids := make([]snapshot.SourceID, 0, len(blocks))
	for _, b := range blocks {
		live[b.ID] = true
		ids = append(ids, b.ID)
	}
	for _, unsupported := range e.Report.Unsupported {
		for _, id := range unsupported.BlockIDs {
			live[id] = true
		}
	}
	records, failed, err := store.ForegroundRecords(ids)
	if err != nil {
		e.Report.ForegroundSkipped = append(e.Report.ForegroundSkipped, skipBlocks(blocks, "foreground records: "+bounded(err.Error(), 200))...)
		return nil, false
	}
	for _, b := range blocks {
		if err := failed[b.ID]; err != nil {
			e.Report.ForegroundSkipped = append(e.Report.ForegroundSkipped, snapshot.Skip{BlockID: b.ID, Reason: "foreground record: " + bounded(err.Error(), 200)})
			continue
		}
		b.Foreground = records[b.ID]
	}
	return live, true
}
