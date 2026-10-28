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
	// maxForegroundRecord bounds one record file: the command and a newline.
	maxForegroundRecord = snapshot.MaxForeground + 1
	// foregroundGrace keeps a record or temporary file a hook wrote moments
	// ago, so a pane created during a capture does not lose its record.
	foregroundGrace = time.Minute
)

// ForegroundRecords reads the record of every block in blocks: the command
// its shell hook wrote when a foreground command started, and removed when it
// finished. Hooks write records atomically and without the state lock, so a
// read never sees a partial file. A missing record is nothing; a record that
// cannot be used is returned in failed. The directory itself must be private.
func (s *Store) ForegroundRecords(blocks []snapshot.SourceID) (records map[snapshot.SourceID]string, failed map[snapshot.SourceID]error, err error) {
	dir, ok, err := s.optionalPrivateDir(foregroundDir)
	if err != nil || !ok {
		return nil, nil, err
	}
	records, failed = make(map[snapshot.SourceID]string), make(map[snapshot.SourceID]error)
	for _, id := range blocks {
		// Only a well-formed block ID names a record; nothing else may become
		// part of a path under the records directory.
		if !receipt.ValidBlockID(string(id)) {
			continue
		}
		uuid := strings.TrimPrefix(string(id), "block:")
		data, err := readBounded(filepath.Join(dir, uuid), filepath.Join(foregroundDir, uuid), maxForegroundRecord)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		command := strings.TrimSpace(string(data))
		if err == nil {
			err = snapshot.ValidForeground(command)
		}
		if err != nil {
			failed[id] = err
			continue
		}
		records[id] = command
	}
	return records, failed, nil
}

// PruneForeground removes records of blocks that are no longer live and
// temporary files a hook left behind, both only once they are older than the
// grace period.
func (s *Store) PruneForeground(live map[snapshot.SourceID]bool) error {
	dir, ok, err := s.optionalPrivateDir(foregroundDir)
	if err != nil || !ok {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || live[snapshot.SourceID("block:"+name)] {
			continue
		}
		if !receipt.ValidBlockID("block:"+name) && !strings.HasPrefix(name, tempPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < foregroundGrace {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
