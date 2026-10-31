package state

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

// The delivery mailbox is how rexurrect and the shell hook in a pane talk
// without rexurrect typing anything. Under <state>/deliver/<block uuid>/ the
// hook publishes `ready` ("<pid> <shell> <hook id>") once its handler is
// installed and removes the mailbox when the shell exits; rexurrect
// publishes `request` ("<attempt> <hook id> <mode>\n<command>\n") and wakes
// the hook; the hook claims the request by renaming it, so exactly one of
// the hook and rexurrect ever owns it, checks its own line buffer, and
// publishes `ack` ("<attempt> <outcome>") with outcome delivered:run,
// delivered:type, skipped:pending_input or rejected:other_hook. The attempt
// binds request and acknowledgement to one delivery record, and the hook id
// binds them to one shell lifetime, so neither a late acknowledgement nor a
// replaced shell can be mistaken for the current attempt. A zsh hook also
// keeps `fifo` open, which is how it is woken.
const (
	deliverDir     = "deliver"
	mailboxReady   = "ready"
	mailboxRequest = "request"
	mailboxAck     = "ack"
	mailboxFIFO    = "fifo"
	maxMailboxFile = 8192
	mailboxGrace   = time.Minute
	// DeliveryModeRun asks the hook to run the command.
	DeliveryModeRun = "run"
	// DeliveryModeType puts the command at the prompt without running it.
	DeliveryModeType = "type"
)

var (
	hookIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	attemptPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// DeliveryHook is what a shell hook announced about itself: the shell's
// pid, its name, and an id that changes with every hook installation.
type DeliveryHook struct {
	PID   int
	Shell string
	ID    string
}

// DeliveryAck is the hook's answer to one attempt.
type DeliveryAck struct {
	Attempt string
	Outcome string
}

// NewDeliveryAttempt names one delivery attempt.
func NewDeliveryAttempt() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(entropy[:]), nil
}

// ValidDeliveryAttempt reports whether attempt came from NewDeliveryAttempt.
func ValidDeliveryAttempt(attempt string) bool { return attemptPattern.MatchString(attempt) }

// mailbox resolves block's mailbox directory: ok is false when the hook has
// not created it, and an existing one must be private.
func (s *Store) mailbox(block snapshot.SourceID) (path string, ok bool, err error) {
	if !receipt.ValidBlockID(string(block)) {
		return "", false, fmt.Errorf("invalid block ID %q", block)
	}
	parent, ok, err := s.optionalPrivateDir(deliverDir)
	if err != nil || !ok {
		return "", false, err
	}
	uuid := strings.TrimPrefix(string(block), "block:")
	path = filepath.Join(parent, uuid)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return path, false, nil
	}
	return path, true, privateDirectory(path, filepath.Join(deliverDir, uuid))
}

// DeliveryHook reads what the hook in block's pane announced: ok is false
// when no hook has announced itself.
func (s *Store) DeliveryHook(block snapshot.SourceID) (DeliveryHook, bool, error) {
	dir, ok, err := s.mailbox(block)
	if err != nil || !ok {
		return DeliveryHook{}, false, err
	}
	data, err := readBounded(filepath.Join(dir, mailboxReady), mailboxReady, maxMailboxFile)
	if errors.Is(err, os.ErrNotExist) {
		return DeliveryHook{}, false, nil
	}
	if err != nil {
		return DeliveryHook{}, false, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 {
		return DeliveryHook{}, false, errors.New("malformed delivery hook announcement")
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 || !bounded(fields[1], 64) || !hookIDPattern.MatchString(fields[2]) {
		return DeliveryHook{}, false, errors.New("malformed delivery hook announcement")
	}
	return DeliveryHook{PID: pid, Shell: fields[1], ID: fields[2]}, true, nil
}

// WriteDeliveryRequest publishes the request of attempt for the hook that
// announced itself as hookID.
func (s *Store) WriteDeliveryRequest(block snapshot.SourceID, attempt, hookID, mode, command string) error {
	if !ValidDeliveryAttempt(attempt) || !hookIDPattern.MatchString(hookID) || mode != DeliveryModeRun && mode != DeliveryModeType || !bounded(command, maxDeliveryInput) || strings.ContainsAny(command, "\n\r\x00") {
		return errors.New("invalid delivery request")
	}
	dir, ok, err := s.mailbox(block)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("delivery mailbox does not exist")
	}
	return publishFile(filepath.Join(dir, mailboxRequest), mailboxRequest, []byte(attempt+" "+hookID+" "+mode+"\n"+command+"\n"))
}

// RevokeDeliveryRequest removes the request if the hook has not claimed it.
// The hook claims by renaming the file, so exactly one side succeeds:
// revoked is true only when this call removed it, which proves the hook
// never saw it. A request that is already gone, or a mailbox that is gone
// with it, proves nothing.
func (s *Store) RevokeDeliveryRequest(block snapshot.SourceID) (revoked bool, err error) {
	dir, ok, err := s.mailbox(block)
	if err != nil || !ok {
		return false, err
	}
	err = os.Remove(filepath.Join(dir, mailboxRequest))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// DeliveryAck reads the hook's latest acknowledgement, if it has written
// one; the caller matches its attempt.
func (s *Store) DeliveryAck(block snapshot.SourceID) (DeliveryAck, bool, error) {
	dir, ok, err := s.mailbox(block)
	if err != nil || !ok {
		return DeliveryAck{}, false, err
	}
	data, err := readBounded(filepath.Join(dir, mailboxAck), mailboxAck, maxMailboxFile)
	if errors.Is(err, os.ErrNotExist) {
		return DeliveryAck{}, false, nil
	}
	if err != nil {
		return DeliveryAck{}, false, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || !ValidDeliveryAttempt(fields[0]) || !bounded(fields[1], 64) {
		return DeliveryAck{}, false, errors.New("malformed delivery acknowledgement")
	}
	return DeliveryAck{Attempt: fields[0], Outcome: fields[1]}, true, nil
}

// DeliveryFIFO is the path of the fifo a zsh hook listens on, when the
// mailbox holds one.
func (s *Store) DeliveryFIFO(block snapshot.SourceID) (string, bool, error) {
	dir, ok, err := s.mailbox(block)
	if err != nil || !ok {
		return "", false, err
	}
	path := filepath.Join(dir, mailboxFIFO)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeNamedPipe == 0 || info.Mode().Perm() != 0600 {
		return "", false, errors.New("delivery fifo must be a private named pipe")
	}
	return path, true, nil
}

// DeliveryHooksInUse reports whether any pane's hook has created a mailbox:
// the sign that the user installed the hook at all.
func (s *Store) DeliveryHooksInUse() (bool, error) {
	dir, ok, err := s.optionalPrivateDir(deliverDir)
	if err != nil || !ok {
		return false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() && receipt.ValidBlockID("block:"+entry.Name()) {
			return true, nil
		}
	}
	return false, nil
}

// PruneDeliveryMailboxes removes the mailboxes of blocks that are no longer
// live, once they are older than the grace period: a hook whose shell was
// killed with the server never got to remove its own.
func (s *Store) PruneDeliveryMailboxes(live map[snapshot.SourceID]bool) error {
	dir, ok, err := s.optionalPrivateDir(deliverDir)
	if err != nil || !ok {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !receipt.ValidBlockID("block:"+name) || live[snapshot.SourceID("block:"+name)] {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < mailboxGrace {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
