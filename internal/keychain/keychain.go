// Package keychain keeps the scrollback master keys in the user's macOS login
// keychain through /usr/bin/security. The security tool is the item's only
// trusted application, so upgrading rexurrect never loses access; see
// docs/.ai/evidence/2026-10-21-scrollback-backend-probes.md for the measured
// behaviour this package encodes.
package keychain

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds every security call. A locked keychain does not
	// fail; it prompts and blocks, so the deadline is what turns that into a
	// reported "unavailable" instead of a hung automatic cycle.
	DefaultTimeout = 3 * time.Second
	// KeySize is the only key length the store accepts.
	KeySize = 32

	defaultSecurity = "/usr/bin/security"
	account         = "rexurrect"
	notFoundExit    = 44 // errSecItemNotFound as reported by the security tool
)

var (
	// ErrNotFound means no item exists for the key ID.
	ErrNotFound = errors.New("keychain item not found")
	// ErrUnavailable means the keychain did not answer within the deadline,
	// typically because it is locked and macOS is prompting for its password.
	ErrUnavailable = errors.New("keychain unavailable")

	keyIDPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	servicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Store names one keychain service. Each key ID becomes one generic-password
// item under that service with account "rexurrect".
type Store struct {
	Service  string
	Security string        // Path of the security tool; empty uses /usr/bin/security.
	Timeout  time.Duration // Zero uses DefaultTimeout.
}

// validKeyID reports whether id is the 32-hex-digit form every key ID uses.
func validKeyID(id string) bool { return keyIDPattern.MatchString(id) }

func (s Store) validate(keyID string) error {
	if !servicePattern.MatchString(s.Service) {
		return errors.New("keychain service must be a bounded token")
	}
	if !validKeyID(keyID) {
		return errors.New("keychain key ID must be 32 hex digits")
	}
	return nil
}

func (s Store) security() string {
	if s.Security == "" {
		return defaultSecurity
	}
	return s.Security
}

func (s Store) timeout() time.Duration {
	if s.Timeout <= 0 {
		return DefaultTimeout
	}
	return s.Timeout
}

// Load returns the key stored under keyID. ErrNotFound and ErrUnavailable are
// distinguishable with errors.Is; any other failure carries the tool's stderr.
func (s Store) Load(ctx context.Context, keyID string) ([]byte, error) {
	if err := s.validate(keyID); err != nil {
		return nil, err
	}
	out, err := s.run(ctx, nil, "find-generic-password", "-a", account, "-s", s.Service+"."+keyID, "-w")
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimRight(string(out), "\r\n"))
	if err != nil || len(key) != KeySize {
		return nil, errors.New("keychain item does not hold a 32-byte key")
	}
	return key, nil
}

// Create stores key under a new keyID. It never overwrites: the security tool
// refuses a duplicate item and that failure is returned. The key reaches the
// tool on stdin through its interactive mode, never on the command line.
func (s Store) Create(ctx context.Context, keyID string, key []byte) error {
	if err := s.validate(keyID); err != nil {
		return err
	}
	if len(key) != KeySize {
		return errors.New("keychain key must be 32 bytes")
	}
	command := fmt.Sprintf("add-generic-password -a %s -s %s -l rexurrect-scrollback -w %s\n",
		account, s.Service+"."+keyID, base64.StdEncoding.EncodeToString(key))
	_, err := s.run(ctx, []byte(command), "-i")
	return err
}

// run executes one bounded security command. The process is killed at the
// deadline so a password prompt cannot hold the caller.
func (s Store) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, s.security(), args...)
	cmd.WaitDelay = time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w: security %s did not answer within %s", ErrUnavailable, args[0], s.timeout())
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), nil
	case errors.As(err, &exit) && exit.ExitCode() == notFoundExit:
		return nil, ErrNotFound
	case errors.As(err, &exit):
		return nil, fmt.Errorf("security %s exited %d: %s", args[0], exit.ExitCode(), strings.TrimSpace(stderr.String()))
	default:
		return nil, fmt.Errorf("security %s: %w", args[0], err)
	}
}
