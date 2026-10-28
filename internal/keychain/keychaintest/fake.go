// Package keychaintest provides a security(1) stand-in for tests: items live
// in files under a private directory, so key provisioning, loss, and hangs
// can be driven without touching a real keychain.
package keychaintest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/keychain"
)

const script = `#!/bin/sh
[ -z "${RR_FAKE_HANG:-}" ] || sleep 5
items="$RR_ITEMS"
if [ "$1" = -i ]; then
  read -r line
  set -- $line
  svc=""; val=""
  while [ $# -gt 0 ]; do case "$1" in -s) svc=$2; shift 2;; -w) val=$2; shift 2;; *) shift;; esac; done
  [ -f "$items/$svc" ] && exit 45
  printf '%s\n' "$val" > "$items/$svc"; exit 0
fi
svc=""
while [ $# -gt 0 ]; do case "$1" in -s) svc=$2; shift 2;; *) shift;; esac; done
[ -f "$items/$svc" ] || exit 44
cat "$items/$svc"
`

// Fake is a keychain store backed by files. Items returns the directory
// holding them, one file per service name, so a test can delete a key.
type Fake struct {
	keychain.Store
	Items string
}

// New installs the fake and points RR_ITEMS at its item directory for the
// test's lifetime. Set RR_FAKE_HANG=1 to make every call block past the
// store's deadline.
func New(t testing.TB, service string) Fake {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "security")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	items := filepath.Join(dir, "items")
	if err := os.Mkdir(items, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RR_ITEMS", items)
	Warm(t, path)
	return Fake{Store: keychain.Store{Service: service, Security: path, Timeout: 300 * time.Millisecond}, Items: items}
}

// Warm runs a freshly written fake once with a harmless environment: macOS
// checks a new executable on its first launch, which alone can exceed the
// store's deadline, and that latency must not count as a keychain hang.
func Warm(t testing.TB, path string) {
	t.Helper()
	cmd := exec.Command(path)
	cmd.Env = []string{"RR_ITEMS=/dev/null", "RR_ARGV=/dev/null", "RR_STDIN=/dev/null"}
	if _, err := cmd.CombinedOutput(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("warm fake security: %v", err)
		}
	}
}

// Remove deletes the item for keyID, simulating a key lost from the keychain.
func (f Fake) Remove(t testing.TB, keyID string) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.Items, f.Service+"."+keyID)); err != nil {
		t.Fatal(err)
	}
}
