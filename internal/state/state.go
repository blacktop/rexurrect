// Package state owns the private, durable snapshot generation directory.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

const (
	FormatVersion = 1
	MaxFileSize   = 16 << 20
)

var (
	ErrNoCurrent = errors.New("state has no current generation")
	ErrLocked    = errors.New("state is locked")
)

type Envelope struct {
	FormatVersion int               `json:"format_version"`
	Generation    uint64            `json:"generation"`
	CapturedAt    time.Time         `json:"captured_at"`
	RexVersion    string            `json:"rex_version"`
	Commit        string            `json:"commit"`
	HostID        string            `json:"host_id"`
	InstanceID    string            `json:"instance_id"`
	Snapshot      snapshot.Snapshot `json:"snapshot"`
	Report        *snapshot.Report  `json:"report"`
}

func (e Envelope) Validate() error {
	if e.FormatVersion != FormatVersion {
		return fmt.Errorf("unsupported envelope format version %d", e.FormatVersion)
	}
	if e.Generation == 0 || e.CapturedAt.IsZero() || !bounded(e.RexVersion, 4096) || !bounded(e.Commit, 4096) || !bounded(e.HostID, 256) || !bounded(e.InstanceID, 256) {
		return errors.New("invalid envelope metadata")
	}
	if e.Snapshot.Version < 1 || e.Snapshot.Version > snapshot.Version {
		return fmt.Errorf("unsupported snapshot version %d", e.Snapshot.Version)
	}
	ids, err := e.Snapshot.Inventory()
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if e.Report == nil || len(e.Report.Unsupported) > 16384 || len(e.Report.PrunedReceipts) > MaxReceipts || len(e.Report.CWDSubstitutions) > len(ids) {
		return errors.New("missing or excessive unsupported report")
	}
	if e.Snapshot.Version < 2 && e.Report.PrunedReceipts != nil {
		return errors.New("receipt pruning requires snapshot version 2")
	}
	pruned := make(map[snapshot.SourceID]bool)
	for _, id := range e.Report.PrunedReceipts {
		if !receipt.ValidBlockID(string(id)) || pruned[id] {
			return errors.New("invalid pruned receipt ID")
		}
		pruned[id] = true
	}
	if e.Snapshot.Version < 2 && e.Report.CWDSubstitutions != nil {
		return errors.New("capture cwd substitutions require snapshot version 2")
	}
	if e.Snapshot.Version < 4 && e.Report.ScrollbackSkipped != nil {
		return errors.New("scrollback skips require snapshot version 4")
	}
	if len(e.Report.ScrollbackSkipped) > len(ids) {
		return errors.New("excessive scrollback skips")
	}
	for _, skip := range e.Report.ScrollbackSkipped {
		if ids[skip.BlockID] != "block" || !bounded(skip.Reason, 256) {
			return errors.New("invalid scrollback skip")
		}
	}
	// Match each substitution to the cwd actually stored for that block.
	cwds := make(map[snapshot.SourceID]string)
	if len(e.Report.CWDSubstitutions) != 0 {
		for _, session := range e.Snapshot.Sessions {
			for _, b := range session.Blocks() {
				cwds[b.ID] = b.CWD
			}
		}
		for _, sub := range e.Report.CWDSubstitutions {
			if cwd, ok := cwds[sub.BlockID]; !ok || cwd != sub.CWD {
				return errors.New("invalid capture cwd substitution")
			}
			delete(cwds, sub.BlockID)
		}
	}
	windows := make(map[snapshot.SourceID]snapshot.SourceID)
	for _, s := range e.Snapshot.Sessions {
		for _, w := range s.Windows {
			windows[w.ID] = s.ID
		}
	}
	add := func(id snapshot.SourceID, kind string) error {
		if !bounded(string(id), 256) || ids[id] != "" || len(ids) >= 32768 {
			return fmt.Errorf("invalid or duplicate unsupported %s ID %q", kind, id)
		}
		ids[id] = kind
		return nil
	}
	for _, u := range e.Report.Unsupported {
		if ids[u.SessionID] != "session" || !bounded(u.Kind, 256) || !bounded(u.Reason, 4096) || len(u.Flavor) > 4096 || len(u.BlockIDs) > 16384 {
			return errors.New("invalid unsupported entry")
		}
		if u.Kind == "detached" {
			if u.WindowID != "" || u.LayerID != "" || len(u.BlockIDs) != 1 || u.Flavor == "" {
				return errors.New("invalid unsupported detached block")
			}
		} else {
			if u.Kind == "tiled" || windows[u.WindowID] != u.SessionID {
				return errors.New("invalid unsupported layer reference")
			}
			if err := add(u.LayerID, "layer"); err != nil {
				return err
			}
		}
		for _, id := range u.BlockIDs {
			if err := add(id, "block"); err != nil {
				return err
			}
		}
	}
	return nil
}

func bounded(s string, max int) bool { return s != "" && len(s) <= max }

type pointer struct {
	Generation    uint64 `json:"generation"`
	LastKnownGood uint64 `json:"last_known_good"`
}

type Current struct {
	Envelope      Envelope
	LastKnownGood *Envelope
}

// Store holds an exclusive OS lock until Close. The persistent lock file must
// never be unlinked: doing so would let another process lock a different inode.
type Store struct {
	dir  string
	lock *os.File
}

func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("state directory is required")
	}
	// Paths derived from the store are embedded in commands that run in
	// other working directories, so the store is absolute by construction.
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	if err := makeDirectory(dir); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := privateDirectory(dir, "state directory"); err != nil {
		return nil, err
	}
	f, err := openFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: %w", ErrLocked, err), f.Close())
	}
	return &Store{dir: dir, lock: f}, nil
}

func (s *Store) Close() error { return s.lock.Close() }

// Dir is the state directory this store locks.
func (s *Store) Dir() string { return s.dir }

// OpenWait is the bounded lock acquisition path for short-lived receipt hooks.
// Save, restore, and auto retain Open's nonblocking exclusion behavior.
func OpenWait(ctx context.Context, dir string, timeout time.Duration) (*Store, error) {
	if timeout <= 0 {
		return nil, errors.New("state lock timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("waiting for state lock: %w: %w", ErrLocked, err)
		}
		store, err := Open(dir)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return store, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func makeDirectory(dir string) error {
	var missing []string
	for path := filepath.Clean(dir); ; path = filepath.Dir(path) {
		if _, err := os.Stat(path); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, path)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Sync each new directory's name in its parent, including the first one
	// created below an existing ancestor. Syncing only the leaf is insufficient.
	for _, m := range slices.Backward(missing) {
		if err := syncDirectory(filepath.Dir(m)); err != nil {
			return err
		}
	}
	return nil
}

// privateDirectory requires path to be a real directory with mode 0700. Lstat
// never follows a symlink, so nothing written beneath a checked path can leave
// the state directory.
func privateDirectory(path, name string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("%s must be a private directory (mode 0700)", name)
	}
	return nil
}

// subdirectory creates the nested directory named by parts under the state
// directory, checking each component in turn so a planted symlink is refused
// before anything is created or written below it.
func (s *Store) subdirectory(parts ...string) (string, error) {
	path, name := s.dir, ""
	for _, part := range parts {
		path, name = filepath.Join(path, part), filepath.Join(name, part)
		if err := makeDirectory(path); err != nil {
			return "", err
		}
		if err := privateDirectory(path, name); err != nil {
			return "", err
		}
	}
	return path, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func openFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0600) {
		err = errors.New("state file must be regular and private (mode 0600)")
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func generationName(n uint64) string { return fmt.Sprintf("generation-%020d.json", n) }

func (s *Store) read(name string, limit int64, value any) error {
	return readJSON(filepath.Join(s.dir, name), name, limit, value)
}

// readBounded reads a private regular file of at most limit bytes.
func readBounded(path, name string, limit int64) ([]byte, error) {
	f, err := openFile(path, os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d-byte size limit", name, limit)
	}
	return data, nil
}

func readJSON(path, name string, limit int64, value any) error {
	data, err := readBounded(path, name, limit)
	if err != nil {
		return err
	}
	if err := rejectDuplicateMembers(data); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode %s: unexpected trailing JSON (%v)", name, err)
	}
	return nil
}

func (s *Store) load(n uint64) (Envelope, error) {
	var e Envelope
	if err := s.read(generationName(n), MaxFileSize, &e); err != nil {
		return e, err
	}
	if err := e.Validate(); err != nil {
		return e, fmt.Errorf("generation %d: %w", n, err)
	}
	if e.Generation != n {
		return e, fmt.Errorf("generation %d contains generation %d", n, e.Generation)
	}
	return e, nil
}

func (s *Store) LoadCurrent() (Current, error) {
	var p pointer
	if err := s.read("current.json", 1024, &p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Current{}, ErrNoCurrent
		}
		return Current{}, err
	}
	if p.Generation == 0 || (p.Generation == 1 && p.LastKnownGood != 0) || (p.Generation > 1 && p.LastKnownGood != p.Generation-1) {
		return Current{}, errors.New("invalid current generation pointer")
	}
	e, err := s.load(p.Generation)
	if err != nil {
		return Current{}, err
	}
	current := Current{Envelope: e}
	if p.LastKnownGood != 0 {
		previous, err := s.load(p.LastKnownGood)
		if err != nil {
			return Current{}, fmt.Errorf("last-known-good: %w", err)
		}
		current.LastKnownGood = &previous
	}
	return current, nil
}

// PublishGuards reports the predictable reasons Publish would refuse e over
// current: an exhausted generation counter, a different Rex host, or an empty
// snapshot over a nonempty history without allowEmpty. Callers with optional
// side effects run it before them, on the Current they already hold.
func PublishGuards(current Current, e Envelope, allowEmpty bool) error {
	previous := current.Envelope.Generation
	if previous == math.MaxUint64 {
		return errors.New("generation number exhausted")
	}
	if previous != 0 && current.Envelope.HostID != e.HostID {
		return errors.New("state belongs to a different Rex host")
	}
	if !allowEmpty && len(e.Snapshot.Sessions) == 0 && (len(current.Envelope.Snapshot.Sessions) != 0 || current.LastKnownGood != nil && len(current.LastKnownGood.Snapshot.Sessions) != 0) {
		return errors.New("refusing empty snapshot over nonempty last-known-good; use --allow-empty")
	}
	return nil
}

// Publish validates before writing anything. A single pointer rename commits
// the new generation and the previous, validated generation together. Older
// committed generations are pruned only after the pointer is durable.
func (s *Store) Publish(e Envelope, allowEmpty bool) (Envelope, error) {
	current, err := s.LoadCurrent()
	if err != nil && !errors.Is(err, ErrNoCurrent) {
		return Envelope{}, err
	}
	if err := PublishGuards(current, e, allowEmpty); err != nil {
		return Envelope{}, err
	}
	previous := current.Envelope.Generation
	e.FormatVersion, e.Generation = FormatVersion, previous+1
	if e.Snapshot.Version != snapshot.Version {
		// Validate the old schema before upgrading the version we publish.
		if err := e.Validate(); err != nil {
			return Envelope{}, err
		}
		e.Snapshot.Version = snapshot.Version
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	name := generationName(e.Generation)
	if _, err := os.Lstat(filepath.Join(s.dir, name)); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return Envelope{}, err
		}
		return Envelope{}, fmt.Errorf("unpublished generation already exists: %s", name)
	}
	if err := s.write(name, e); err != nil {
		return Envelope{}, err
	}
	if err := s.write("current.json", pointer{Generation: e.Generation, LastKnownGood: previous}); err != nil {
		return Envelope{}, err
	}
	if err := s.pruneGenerations(previous); err != nil {
		return e, fmt.Errorf("generation %d was published, but pruning older generations failed: %w", e.Generation, err)
	}
	return e, nil
}

func (s *Store) pruneGenerations(before uint64) (err error) {
	if before < 2 {
		return nil
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	removed := false
	defer func() {
		if removed {
			err = errors.Join(err, dir.Sync())
		}
		err = errors.Join(err, dir.Close())
	}()
	for {
		// Bound memory while cleaning up history from older versions.
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			name := entry.Name()
			n, parseErr := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "generation-"), ".json"), 10, 64)
			if parseErr != nil || n == 0 || n >= before || name != generationName(n) || !entry.Type().IsRegular() {
				continue
			}
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
				return fmt.Errorf("remove %s: %w", name, err)
			}
			removed = true
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (s *Store) write(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) >= MaxFileSize {
		return fmt.Errorf("%s exceeds %d-byte size limit", name, MaxFileSize)
	}
	return publishFile(filepath.Join(s.dir, name), name, append(data, '\n'))
}

// publishFile publishes data at target through a private temporary file beside
// it, fsyncs the file and then the directory, so a nested record is durable
// where it lives before the caller's next side effect.
func publishFile(target, name string, data []byte) error {
	dir := filepath.Dir(target)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer func() { _ = os.Remove(temp) }()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(temp, target); err != nil {
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return syncDirectory(dir)
}
