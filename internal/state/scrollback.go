package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blacktop/rexurrect/internal/keychain"
	"github.com/blacktop/rexurrect/internal/scrollback"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

const (
	scrollbackDir       = "scrollback"
	scrollbackStateName = "state.json"
	scrollbackPinsDir   = "pins"
	scrollbackSuffix    = ".rrsb"
	// MaxScrollbackBlob bounds one sidecar: the sealed form of 1 MiB.
	MaxScrollbackBlob = 2 << 20
	// ScrollbackPinExpiry bounds how long a restore's pin protects its
	// generation's sidecars if the restore never records completion.
	ScrollbackPinExpiry = time.Hour
)

// ErrScrollbackKeyMissing means the state store finished provisioning a key
// that the keychain no longer returns. It is never regenerated silently.
var ErrScrollbackKeyMissing = errors.New("scrollback key is missing from the keychain")

// scrollbackState is the nonsecret half of the key lifecycle: which key ID
// this state store uses and whether its keychain item was finalized.
type scrollbackState struct {
	FormatVersion int    `json:"format_version"`
	StateID       string `json:"state_id"`
	KeyID         string `json:"key_id"`
	Status        string `json:"status"` // pending until the keychain item is known to exist
}

// stateID decodes the binding state ID; it shares the key ID grammar.
func (r scrollbackState) stateID() ([16]byte, error) {
	id, err := scrollback.ParseKeyID(r.StateID)
	if err != nil {
		return [16]byte{}, errors.New("invalid scrollback state ID")
	}
	return [16]byte(id), nil
}

// ScrollbackKey returns this store's binding state ID, key ID, and master key,
// provisioning them on first use. Provisioning is transactional: a pending
// record is written before the keychain item, and a pending record whose item
// already exists is finalized rather than replaced.
func (s *Store) ScrollbackKey(ctx context.Context, kc keychain.Store) ([16]byte, scrollback.KeyID, []byte, error) {
	var stateID [16]byte
	var keyID scrollback.KeyID
	record, err := s.readScrollbackState()
	if errors.Is(err, os.ErrNotExist) {
		record, err = s.provisionScrollbackState()
	}
	if err != nil {
		return stateID, keyID, nil, err
	}
	if stateID, err = record.stateID(); err != nil {
		return stateID, keyID, nil, err
	}
	if keyID, err = scrollback.ParseKeyID(record.KeyID); err != nil {
		return stateID, keyID, nil, err
	}
	master, err := kc.Load(ctx, record.KeyID)
	switch {
	case errors.Is(err, keychain.ErrNotFound) && record.Status == "ready":
		return stateID, keyID, nil, fmt.Errorf("%w: key %s", ErrScrollbackKeyMissing, record.KeyID)
	case errors.Is(err, keychain.ErrNotFound):
		master = make([]byte, scrollback.KeySize)
		if _, err := rand.Read(master); err != nil {
			return stateID, keyID, nil, err
		}
		if err := kc.Create(ctx, record.KeyID, master); err != nil {
			return stateID, keyID, nil, fmt.Errorf("provision scrollback key: %w", err)
		}
	case err != nil:
		return stateID, keyID, nil, err
	}
	// The item is known to exist now, whether it was just created or found.
	if record.Status != "ready" {
		record.Status = "ready"
		if err := s.writeScrollbackState(record); err != nil {
			return stateID, keyID, nil, err
		}
	}
	return stateID, keyID, master, nil
}

func (s *Store) provisionScrollbackState() (scrollbackState, error) {
	var ids [32]byte
	if _, err := rand.Read(ids[:]); err != nil {
		return scrollbackState{}, err
	}
	record := scrollbackState{FormatVersion: 1, StateID: hex.EncodeToString(ids[:16]), KeyID: hex.EncodeToString(ids[16:]), Status: "pending"}
	return record, s.writeScrollbackState(record)
}

func (s *Store) readScrollbackState() (scrollbackState, error) {
	return readScrollbackState(s.dir)
}

// readScrollbackState reads and validates the record under dir; the lock-free
// replay path uses it too, so both readers enforce the same checks.
func readScrollbackState(dir string) (scrollbackState, error) {
	var record scrollbackState
	name := filepath.Join(scrollbackDir, scrollbackStateName)
	if err := readJSON(filepath.Join(dir, name), name, 1024, &record); err != nil {
		return record, err
	}
	_, stateErr := record.stateID()
	_, keyErr := scrollback.ParseKeyID(record.KeyID)
	if record.FormatVersion != 1 || stateErr != nil || keyErr != nil || record.Status != "pending" && record.Status != "ready" {
		return record, errors.New("invalid scrollback state record")
	}
	return record, nil
}

func (s *Store) writeScrollbackState(record scrollbackState) error {
	if _, err := s.subdirectory(scrollbackDir); err != nil {
		return err
	}
	return s.write(filepath.Join(scrollbackDir, scrollbackStateName), record)
}

func scrollbackPath(ref snapshot.ScrollbackRef) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	return filepath.Join(scrollbackDir, ref.KeyID, ref.ObjectID+scrollbackSuffix), nil
}

// HasScrollback reports whether a sidecar exists for ref. Callers must still
// authenticate it with scrollback.Open before relying on it.
func (s *Store) HasScrollback(ref snapshot.ScrollbackRef) bool {
	name, err := scrollbackPath(ref)
	if err != nil {
		return false
	}
	info, err := os.Lstat(filepath.Join(s.dir, name))
	return err == nil && info.Mode().IsRegular()
}

// WriteScrollback publishes an immutable sidecar: ciphertext goes to a private
// temporary file beside its final name, then is renamed into place. An
// existing sidecar is never overwritten.
func (s *Store) WriteScrollback(ref snapshot.ScrollbackRef, blob []byte) error {
	name, err := scrollbackPath(ref)
	if err != nil {
		return err
	}
	if len(blob) == 0 || len(blob) > MaxScrollbackBlob {
		return errors.New("scrollback sidecar has an invalid size")
	}
	if s.HasScrollback(ref) {
		return nil
	}
	if _, err := s.subdirectory(scrollbackDir, ref.KeyID); err != nil {
		return err
	}
	return publishFile(filepath.Join(s.dir, name), name, blob)
}

// ReadScrollback returns the bounded raw sidecar for ref.
func (s *Store) ReadScrollback(ref snapshot.ScrollbackRef) ([]byte, error) {
	return ReadScrollbackSidecar(s.dir, ref)
}

func scrollbackRefs(e Envelope) []string {
	var refs []string
	for _, session := range e.Snapshot.Sessions {
		for _, b := range session.Blocks() {
			if b.Scrollback != nil {
				refs = append(refs, filepath.Join(b.Scrollback.KeyID, b.Scrollback.ObjectID+scrollbackSuffix))
			}
		}
	}
	return refs
}

// PinScrollback records the sidecars a restore of e is about to replay, so
// they survive later publications until the pin expires. The pin lists the
// references itself: the generation file may be pruned long before the hour
// is up. Pins are never released early; the restored shells read their
// sidecars at startup, which no journal record observes.
func (s *Store) PinScrollback(e Envelope) error {
	refs := scrollbackRefs(e)
	if len(refs) == 0 {
		return nil
	}
	if _, err := s.subdirectory(scrollbackDir, scrollbackPinsDir); err != nil {
		return err
	}
	name := filepath.Join(scrollbackDir, scrollbackPinsDir, strconv.FormatUint(e.Generation, 10))
	return s.write(name, refs)
}

// PruneScrollback removes sidecars that neither the retained generations
// (the current one and its last known good) nor an unexpired pin reference.
// Temporary files are removed too; the state lock means no writer can be
// mid-publish. Every directory it deletes from must be a private real
// directory, never a symlink.
func (s *Store) PruneScrollback(retained Current) error {
	root := filepath.Join(s.dir, scrollbackDir)
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := privateDirectory(root, scrollbackDir); err != nil {
		return err
	}
	referenced := retainedScrollback(retained)
	if err := s.pinnedScrollback(referenced); err != nil {
		return err
	}
	keys, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.Type().IsRegular() && strings.HasPrefix(key.Name(), ".tmp-") {
			if err := os.Remove(filepath.Join(root, key.Name())); err != nil {
				return err
			}
			continue
		}
		if _, err := scrollback.ParseKeyID(key.Name()); !key.IsDir() || err != nil {
			continue
		}
		if err := s.pruneScrollbackKey(key.Name(), referenced); err != nil {
			return err
		}
	}
	return nil
}

// retainedScrollback lists the sidecars the retained generations reference.
func retainedScrollback(retained Current) map[string]bool {
	referenced := make(map[string]bool)
	for _, ref := range scrollbackRefs(retained.Envelope) {
		referenced[ref] = true
	}
	if retained.LastKnownGood != nil {
		for _, ref := range scrollbackRefs(*retained.LastKnownGood) {
			referenced[ref] = true
		}
	}
	return referenced
}

// pinnedScrollback adds the sidecars that unexpired pins protect and removes
// expired or malformed pins.
func (s *Store) pinnedScrollback(referenced map[string]bool) error {
	dir := filepath.Join(s.dir, scrollbackDir, scrollbackPinsDir)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := privateDirectory(dir, filepath.Join(scrollbackDir, scrollbackPinsDir)); err != nil {
		return err
	}
	pins, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, pin := range pins {
		_, parseErr := strconv.ParseUint(pin.Name(), 10, 64)
		info, infoErr := pin.Info()
		var refs []string
		if parseErr == nil && infoErr == nil && pin.Type().IsRegular() && time.Since(info.ModTime()) <= ScrollbackPinExpiry {
			if err := s.read(filepath.Join(scrollbackDir, scrollbackPinsDir, pin.Name()), MaxFileSize, &refs); err != nil {
				return err
			}
		}
		if refs == nil {
			if err := os.Remove(filepath.Join(dir, pin.Name())); err != nil {
				return err
			}
			continue
		}
		for _, ref := range refs {
			referenced[ref] = true
		}
	}
	return nil
}

// pruneScrollbackKey removes every unreferenced sidecar and temporary file
// under one key directory.
func (s *Store) pruneScrollbackKey(keyID string, referenced map[string]bool) error {
	dir := filepath.Join(s.dir, scrollbackDir, keyID)
	if err := privateDirectory(dir, filepath.Join(scrollbackDir, keyID)); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		kept := strings.HasSuffix(name, scrollbackSuffix) && referenced[filepath.Join(keyID, name)]
		if kept || !entry.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// ReadScrollbackSidecar reads a sidecar without the state lock. Sidecars are
// immutable and pinned while a restore replays them, so a restored shell's
// replay command can read one while the restoring process still holds the
// lock.
func ReadScrollbackSidecar(dir string, ref snapshot.ScrollbackRef) ([]byte, error) {
	name, err := scrollbackPath(ref)
	if err != nil {
		return nil, err
	}
	return readBounded(filepath.Join(dir, name), name, MaxScrollbackBlob)
}

// ReadScrollbackStateID reads the binding state ID without the state lock,
// for the same replay path.
func ReadScrollbackStateID(dir string) ([16]byte, error) {
	record, err := readScrollbackState(dir)
	if err != nil {
		return [16]byte{}, err
	}
	return record.stateID()
}
