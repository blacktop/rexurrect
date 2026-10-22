package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"

	"github.com/blacktop/rexurrect/internal/keychain"
	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/rex"
	"github.com/blacktop/rexurrect/internal/scrollback"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// keychainStore is the login-keychain store every command shares; tests
// substitute the security tool through REXURRECT_SECURITY_TOOL.
func keychainStore() keychain.Store {
	return keychain.Store{Service: rex.ScrollbackKeychainService, Security: os.Getenv("REXURRECT_SECURITY_TOOL")}
}

// runScrollback implements `scrollback replay`, which a restored block runs
// before its shell starts. It writes the block's decrypted scrollback to
// stdout only after the whole sidecar authenticates, and writes nothing on
// any failure. It takes no state lock: the restoring process holds it while
// the block starts, and sidecars are immutable and pinned.
func runScrollback(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "replay" {
		return errors.New("usage: rexurrect scrollback replay --state-dir DIR --key-id ID --object-id ID --host ID --block ID")
	}
	flags := flag.NewFlagSet("scrollback replay", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("state-dir", "", "private snapshot directory")
	keyID := flags.String("key-id", "", "key ID from the snapshot reference")
	objectID := flags.String("object-id", "", "object ID from the snapshot reference")
	host := flags.String("host", "", "Rex host ID the snapshot was saved from")
	block := flags.String("block", "", "saved source block ID")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *dir == "" || *host == "" || !receipt.ValidBlockID(*block) || flags.NArg() != 0 {
		return errors.New("--state-dir, --host, and a full --block ID are required")
	}
	ref := snapshot.ScrollbackRef{KeyID: *keyID, ObjectID: *objectID}
	parsedKey, parsedObject, err := ref.Parse()
	if err != nil {
		return err
	}
	stateID, err := state.ReadScrollbackStateID(*dir)
	if err != nil {
		return err
	}
	blob, err := state.ReadScrollbackSidecar(*dir, ref)
	if err != nil {
		return err
	}
	master, err := keychainStore().Load(ctx, *keyID)
	if err != nil {
		return err
	}
	plaintext, err := scrollback.Open(master, parsedKey, parsedObject, scrollback.Binding{StateID: stateID, HostID: *host, BlockID: *block}, blob)
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(plaintext, "\x1b[0m\r\n"...))
	return err
}
