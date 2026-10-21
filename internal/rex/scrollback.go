package rex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/blacktop/rexurrect/internal/keychain"
	"github.com/blacktop/rexurrect/internal/scrollback"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

const (
	// ScrollbackKeychainService names the login-keychain items that hold
	// scrollback master keys; `scrollback replay` must use the same name.
	ScrollbackKeychainService = "io.blacktop.rexurrect.scrollback"
	// maxScrollbackPerGeneration bounds the sanitized bytes one save captures.
	maxScrollbackPerGeneration = 32 << 20
)

// replayScript prints a block's decrypted scrollback and then starts the
// shell exactly as Rex does for a default block (see
// docs/.ai/evidence/2026-10-21-scrollback-contract-probe.md §4). Every
// variable part arrives as a positional argument; the decryptor writes to the
// terminal only after authenticating the whole blob and prints nothing on
// failure, so the shell still starts either way.
const replayScript = `printf "\033[H\033[2J\033[3J"; "$1" scrollback replay --state-dir "$2" --key-id "$3" --object-id "$4" --host "$5" --block "$6" 2>/dev/null; printf "\033[0m"; shell=$(dscl . -read "/Users/$7" UserShell | sed "s/^UserShell: //"); exec /usr/bin/login -flp "$7" /bin/bash --noprofile --norc -c 'name=$1; shift; exec -a "-$name" -- "$@"' rex-login "${shell##*/}" "$shell"`

var rexErrorCode = regexp.MustCompile(`\(status=\d+ code=([a-z_]+)\)`)

// rexurrectExecutable locates the binary a restored block runs for its
// replay; tests point it at a freshly built CLI.
var rexurrectExecutable = os.Executable

// rexCode extracts the structured code Rex's CLI appends to an error line.
func rexCode(err error) string {
	if err == nil {
		return ""
	}
	if m := rexErrorCode.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return ""
}

func (c Client) keychainStore() keychain.Store {
	kc := c.Keychain
	if kc.Service == "" {
		kc.Service = ScrollbackKeychainService
	}
	return kc
}

type terminalModes struct {
	Screen string `json:"screen"`
}

// scrollbackSealer holds what every block of one capture shares: the store,
// the key or the reason it is unavailable, the binding's state and host, and
// the remaining generation budget.
type scrollbackSealer struct {
	store   *state.Store
	stateID [16]byte
	keyID   scrollback.KeyID
	master  []byte
	keyErr  error
	hostID  string
	budget  int
}

// captureScrollback adds an encrypted scrollback reference to every shell
// block whose primary-screen output can be captured, and records why the
// others were skipped. Only the caller's cancellation fails the save; a key,
// read, or sidecar problem is a per-block skip. An empty server provisions
// nothing: the key and its state record exist only once a block needs them.
func (c Client) captureScrollback(ctx context.Context, store *state.Store, e *state.Envelope) error {
	if !hasBlocks(e.Snapshot) {
		return nil
	}
	sealer := &scrollbackSealer{store: store, hostID: e.HostID, budget: maxScrollbackPerGeneration}
	sealer.stateID, sealer.keyID, sealer.master, sealer.keyErr = store.ScrollbackKey(ctx, c.keychainStore())
	if sealer.keyErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	for si := range e.Snapshot.Sessions {
		session := &e.Snapshot.Sessions[si]
		for _, b := range session.Blocks() {
			ref, reason, err := c.captureBlock(ctx, sealer, session.ID, b.ID)
			if err != nil {
				return err
			}
			if reason != "" {
				e.Report.ScrollbackSkipped = append(e.Report.ScrollbackSkipped, snapshot.ScrollbackSkip{BlockID: b.ID, Reason: reason})
				continue
			}
			b.Scrollback = ref
		}
	}
	return nil
}

func hasBlocks(s snapshot.Snapshot) bool {
	for _, session := range s.Sessions {
		if len(session.Blocks()) != 0 {
			return true
		}
	}
	return false
}

// captureBlock reads, seals, and publishes one block's scrollback. A nonempty
// reason is a per-block skip; the only error is the context's.
func (c Client) captureBlock(ctx context.Context, sealer *scrollbackSealer, session, block snapshot.SourceID) (*snapshot.ScrollbackRef, string, error) {
	if sealer.keyErr != nil {
		return nil, skipReason(sealer.keyErr), nil
	}
	plaintext, reason, err := c.readScrollback(ctx, snapshot.LiveID(session), snapshot.LiveID(block))
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "scrollback read: " + bounded(err.Error(), 200), nil
	}
	if reason != "" {
		return nil, reason, nil
	}
	if len(plaintext) > sealer.budget {
		return nil, "generation scrollback budget exhausted", nil
	}
	binding := scrollback.Binding{StateID: sealer.stateID, HostID: sealer.hostID, BlockID: string(block)}
	ref, err := sealer.publish(binding, plaintext)
	if err != nil {
		return nil, "scrollback sidecar: " + bounded(err.Error(), 200), nil
	}
	sealer.budget -= len(plaintext)
	return &ref, "", nil
}

func skipReason(err error) string {
	switch {
	case errors.Is(err, state.ErrScrollbackKeyMissing):
		return "scrollback key missing from the keychain"
	case errors.Is(err, keychain.ErrUnavailable):
		return "keychain unavailable"
	default:
		return "scrollback key: " + bounded(err.Error(), 200)
	}
}

func bounded(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// readScrollback captures one block's primary-screen output as sanitized vt
// bytes. The screen is checked before and after the read so a program that
// holds the alternate screen during the read is never captured.
func (c Client) readScrollback(ctx context.Context, session, block snapshot.LiveID) ([]byte, string, error) {
	modes := func() (string, error) {
		var m terminalModes
		err := c.Call(ctx, "com.superlogical.terminal.modes", map[string]any{"session_id": session, "block_id": block, "args": map[string]any{}}, &m)
		return m.Screen, err
	}
	before, err := modes()
	if err != nil {
		return nil, "", err
	}
	if before != "primary" {
		return nil, "alternate screen", nil
	}
	var out struct {
		Content string `json:"content"`
	}
	err = c.Call(ctx, "com.superlogical.terminal.format", map[string]any{"session_id": session, "block_id": block, "args": map[string]any{"format": "vt", "unwrap": true}}, &out)
	if rexCode(err) == "too_large" {
		return nil, "scrollback exceeds Rex's 1 MiB response limit", nil
	}
	if err != nil {
		return nil, "", err
	}
	after, err := modes()
	if err != nil {
		return nil, "", err
	}
	if after != "primary" {
		return nil, "alternate screen", nil
	}
	clean, _ := scrollback.Sanitize([]byte(out.Content))
	if len(strings.TrimSpace(string(clean))) == 0 {
		return nil, "empty", nil
	}
	if len(clean) > scrollback.MaxPlaintext {
		return nil, "scrollback exceeds 1 MiB", nil
	}
	return clean, "", nil
}

// publish reuses an existing sidecar for the same content only after
// authenticating it, and seals a new one otherwise.
func (sc *scrollbackSealer) publish(binding scrollback.Binding, plaintext []byte) (snapshot.ScrollbackRef, error) {
	id, err := scrollback.Identity(sc.master, binding, plaintext)
	if err != nil {
		return snapshot.ScrollbackRef{}, err
	}
	ref := snapshot.ScrollbackRef{KeyID: sc.keyID.String(), ObjectID: id.String()}
	if sc.store.HasScrollback(ref) {
		blob, err := sc.store.ReadScrollback(ref)
		if err == nil {
			if _, err := scrollback.Open(sc.master, sc.keyID, id, binding, blob); err == nil {
				return ref, nil
			}
		}
		return snapshot.ScrollbackRef{}, fmt.Errorf("existing sidecar %s does not authenticate", ref.ObjectID)
	}
	_, blob, err := scrollback.Seal(sc.master, sc.keyID, binding, plaintext)
	if err != nil {
		return snapshot.ScrollbackRef{}, err
	}
	return ref, sc.store.WriteScrollback(ref, blob)
}

// replayCommand is the terminal command a restored block starts with instead
// of Rex's default shell chain when its scrollback can be replayed.
type replayCommand []string

// scrollbackReplay decides, before any session is created, which saved blocks
// can replay their scrollback: the key must load and every sidecar must
// authenticate now. Blocks that cannot are reported and restored as plain
// shells. A successful decision pins the sidecars for the restore; if the pin
// cannot be written, every replay becomes a skip and the restore continues.
func (c Client) scrollbackReplay(ctx context.Context, store *state.Store, dir string, e state.Envelope) (map[snapshot.SourceID]replayCommand, []snapshot.ScrollbackSkip, error) {
	var refs int
	for _, session := range e.Snapshot.Sessions {
		for _, b := range session.Blocks() {
			if b.Scrollback != nil {
				refs++
			}
		}
	}
	if refs == 0 {
		return nil, nil, nil
	}
	var skips []snapshot.ScrollbackSkip
	skipAll := func(reason string) (map[snapshot.SourceID]replayCommand, []snapshot.ScrollbackSkip, error) {
		for _, session := range e.Snapshot.Sessions {
			for _, b := range session.Blocks() {
				if b.Scrollback != nil {
					skips = append(skips, snapshot.ScrollbackSkip{BlockID: b.ID, Reason: reason})
				}
			}
		}
		return nil, skips, nil
	}
	stateID, _, master, err := store.ScrollbackKey(ctx, c.keychainStore())
	if err != nil {
		return skipAll(skipReason(err))
	}
	executable, err := rexurrectExecutable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		return skipAll("rexurrect executable path: " + bounded(err.Error(), 200))
	}
	me, err := user.Current()
	if err != nil || me.Username == "" {
		return skipAll("current user unknown")
	}
	commands := make(map[snapshot.SourceID]replayCommand)
	for _, session := range e.Snapshot.Sessions {
		for _, b := range session.Blocks() {
			ref := b.Scrollback
			if ref == nil {
				continue
			}
			keyID, keyErr := scrollback.ParseKeyID(ref.KeyID)
			objectID, objectErr := scrollback.ParseObjectID(ref.ObjectID)
			blob, readErr := store.ReadScrollback(*ref)
			if err := errors.Join(keyErr, objectErr, readErr); err != nil {
				skips = append(skips, snapshot.ScrollbackSkip{BlockID: b.ID, Reason: "scrollback sidecar unreadable"})
				continue
			}
			binding := scrollback.Binding{StateID: stateID, HostID: e.HostID, BlockID: string(b.ID)}
			if _, err := scrollback.Open(master, keyID, objectID, binding, blob); err != nil {
				skips = append(skips, snapshot.ScrollbackSkip{BlockID: b.ID, Reason: "scrollback sidecar does not authenticate"})
				continue
			}
			commands[b.ID] = replayCommand{"/bin/bash", "--noprofile", "--norc", "-c", replayScript, "rexurrect",
				executable, dir, ref.KeyID, ref.ObjectID, e.HostID, string(b.ID), me.Username}
		}
	}
	if len(commands) == 0 {
		return nil, skips, nil
	}
	// An unpinned replay could read a sidecar that a later publication pruned,
	// so a pin failure restores plain shells instead of failing the recovery.
	if err := store.PinScrollback(e); err != nil {
		reason := "scrollback pin: " + bounded(err.Error(), 200)
		for _, session := range e.Snapshot.Sessions {
			for _, b := range session.Blocks() {
				if _, ok := commands[b.ID]; ok {
					skips = append(skips, snapshot.ScrollbackSkip{BlockID: b.ID, Reason: reason})
				}
			}
		}
		return nil, skips, nil
	}
	return commands, skips, nil
}
