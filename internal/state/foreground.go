package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

const (
	foregroundDir = "foreground"
	// MaxForegroundRecord bounds one record file: the command and a newline.
	MaxForegroundRecord = snapshot.MaxForeground + 1
	// foregroundGrace keeps a record or temporary file a hook wrote moments
	// ago, so a pane created during a capture does not lose its record.
	foregroundGrace = time.Minute
)

// ForegroundRecords reads the record of every block in blocks: the command
// its shell hook wrote when a foreground command started, and removed when it
// finished. Hooks write records atomically and without the state lock, so a
// read never sees a partial file. A missing record is nothing; a record that
// cannot be attached is a per-block skip. The directory itself must be
// private.
func (s *Store) ForegroundRecords(blocks []snapshot.SourceID) (map[snapshot.SourceID]string, []snapshot.Skip, error) {
	dir := filepath.Join(s.dir, foregroundDir)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err := privateDirectory(dir, foregroundDir); err != nil {
		return nil, nil, err
	}
	records := make(map[snapshot.SourceID]string)
	var skips []snapshot.Skip
	for _, id := range blocks {
		uuid, ok := strings.CutPrefix(string(id), "block:")
		if !ok {
			continue
		}
		name := filepath.Join(foregroundDir, uuid)
		data, err := readBounded(filepath.Join(s.dir, name), name, MaxForegroundRecord)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			skips = append(skips, foregroundSkip(id, err))
			continue
		}
		command := strings.TrimSpace(string(data))
		if err := snapshot.ValidForeground(command); err != nil {
			skips = append(skips, foregroundSkip(id, err))
			continue
		}
		records[id] = command
	}
	return records, skips, nil
}

func foregroundSkip(id snapshot.SourceID, err error) snapshot.Skip {
	reason := "foreground record: " + err.Error()
	if len(reason) > 256 {
		reason = reason[:256]
	}
	return snapshot.Skip{BlockID: id, Reason: reason}
}

// PruneForeground removes records of blocks that are no longer live and
// temporary files a hook left behind, both only once they are older than the
// grace period. It returns how many files it removed.
func (s *Store) PruneForeground(live map[snapshot.SourceID]bool) (int, error) {
	dir := filepath.Join(s.dir, foregroundDir)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err := privateDirectory(dir, foregroundDir); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		record := receipt.ValidBlockID("block:" + name)
		if !record && !strings.HasPrefix(name, ".tmp-") {
			continue
		}
		if record && live[snapshot.SourceID("block:"+name)] {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < foregroundGrace {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
