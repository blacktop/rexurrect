package keychain

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testKeyID   = "0123456789abcdef0123456789abcdef"
	testService = "io.blacktop.rexurrect.scrollback"
)

// fakeSecurity records argv and stdin, then behaves as RR_FAKE_MODE says:
// ok (print the stored key), missing (exit 44), hang (sleep past the
// deadline), fail (exit 1 with a message).
func fakeSecurity(t *testing.T) (store Store, argvPath, stdinPath string) {
	t.Helper()
	dir := t.TempDir()
	argvPath, stdinPath = filepath.Join(dir, "argv"), filepath.Join(dir, "stdin")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "$RR_ARGV"
if [ "$1" = -i ]; then cat >> "$RR_STDIN"; fi
case "$RR_FAKE_MODE" in
  ok) printf '%s\n' "$RR_FAKE_KEY" ;;
  missing) printf 'security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.\n' >&2; exit 44 ;;
  hang) sleep 5 ;;
  fail) printf 'security: SecKeychainItemCreateFromContent: The specified item already exists in the keychain.\n' >&2; exit 45 ;;
esac
`
	path := filepath.Join(dir, "security")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RR_ARGV", argvPath)
	t.Setenv("RR_STDIN", stdinPath)
	return Store{Service: testService, Security: path, Timeout: 300 * time.Millisecond}, argvPath, stdinPath
}

func TestLoadAndCreate(t *testing.T) {
	store, argvPath, stdinPath := fakeSecurity(t)
	key := bytes.Repeat([]byte{0xab}, KeySize)
	encoded := base64.StdEncoding.EncodeToString(key)
	t.Setenv("RR_FAKE_MODE", "ok")
	t.Setenv("RR_FAKE_KEY", encoded)
	got, err := store.Load(t.Context(), testKeyID)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("load: %x %v", got, err)
	}
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"find-generic-password", "-a", account, "-s", testService + "." + testKeyID, "-w", ""}, "\n")
	if string(argv) != want {
		t.Fatalf("load argv: %q, want %q", argv, want)
	}
	if err := os.Truncate(argvPath, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(t.Context(), testKeyID, key); err != nil {
		t.Fatal(err)
	}
	argv, _ = os.ReadFile(argvPath)
	if string(argv) != "-i\n" || strings.Contains(string(argv), encoded) {
		t.Fatalf("create argv must be only -i: %q", argv)
	}
	stdin, _ := os.ReadFile(stdinPath)
	wantStdin := "add-generic-password -a " + account + " -s " + testService + "." + testKeyID + " -l rexurrect-scrollback -w " + encoded + "\n"
	if string(stdin) != wantStdin {
		t.Fatalf("create stdin: %q, want %q", stdin, wantStdin)
	}
}

func TestLoadFailures(t *testing.T) {
	store, _, _ := fakeSecurity(t)
	for _, tc := range []struct {
		mode, key string
		want      error
		contains  string
	}{
		{"missing", "", ErrNotFound, ""},
		{"hang", "", ErrUnavailable, "did not answer within"},
		{"fail", "", nil, "exited 45: security: SecKeychainItemCreateFromContent"},
		{"ok", "bm90LWEta2V5", nil, "32-byte key"},
		{"ok", "%%%", nil, "32-byte key"},
	} {
		t.Run(tc.mode+"/"+tc.key, func(t *testing.T) {
			t.Setenv("RR_FAKE_MODE", tc.mode)
			t.Setenv("RR_FAKE_KEY", tc.key)
			started := time.Now()
			got, err := store.Load(t.Context(), testKeyID)
			if got != nil || err == nil {
				t.Fatalf("load returned %x, %v", got, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("load error %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("load error %q lacks %q", err, tc.contains)
			}
			if tc.mode == "hang" && time.Since(started) > 2*time.Second {
				t.Fatalf("deadline not enforced: %s", time.Since(started))
			}
		})
	}
}

func TestCreateFailures(t *testing.T) {
	store, _, _ := fakeSecurity(t)
	key := bytes.Repeat([]byte{1}, KeySize)
	t.Setenv("RR_FAKE_MODE", "fail")
	if err := store.Create(t.Context(), testKeyID, key); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create: %v", err)
	}
	t.Setenv("RR_FAKE_MODE", "hang")
	if err := store.Create(t.Context(), testKeyID, key); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("hung create: %v", err)
	}
	t.Setenv("RR_FAKE_MODE", "ok")
	if err := store.Create(t.Context(), testKeyID, key[:KeySize-1]); err == nil {
		t.Fatal("accepted a short key")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Create(ctx, testKeyID, key); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled context: %v", err)
	}
}

func TestValidation(t *testing.T) {
	store, _, _ := fakeSecurity(t)
	t.Setenv("RR_FAKE_MODE", "ok")
	for _, id := range []string{"", "short", strings.ToUpper(testKeyID), testKeyID + "0", "0123456789abcdef0123456789abcde;"} {
		if _, err := store.Load(t.Context(), id); err == nil {
			t.Errorf("accepted key ID %q", id)
		}
	}
	for _, service := range []string{"", "has space", "semi;colon", strings.Repeat("s", 129)} {
		bad := store
		bad.Service = service
		if _, err := bad.Load(t.Context(), testKeyID); err == nil {
			t.Errorf("accepted service %q", service)
		}
	}
	if !validKeyID(testKeyID) || validKeyID("x") {
		t.Fatal("validKeyID")
	}
	defaults := Store{Service: testService}
	if defaults.security() != defaultSecurity || defaults.timeout() != DefaultTimeout {
		t.Fatal("defaults")
	}
}
