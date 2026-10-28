// Package rex drives a Rex server exclusively through its CLI JSON interface.
package rex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/blacktop/rexurrect/internal/keychain"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxOutput          = 4 << 20
	DefaultCallTimeout = 20 * time.Second
	MaxCallTimeout     = time.Minute
	readOnlyRetries    = 2
	readRetryDelay     = 200 * time.Millisecond
)

var errOutputLimit = errors.New("output from Rex exceeds limit")

type Client struct {
	Binary      string // Resolve once before starting an operation.
	Socket      string
	Env         []string
	CallTimeout time.Duration // Zero uses DefaultCallTimeout.
	LockTimeout time.Duration // Receipt state-lock wait; zero uses DefaultLockTimeout.
	// Scrollback enables encrypted scrollback capture during save. Keychain
	// names the key store; its zero value uses the default service.
	Scrollback bool
	Keychain   keychain.Store
}

// SocketPath extracts the socket from an explicit unix:// endpoint argument.
func SocketPath(server string) (string, error) {
	socket, ok := strings.CutPrefix(server, "unix://")
	if !ok || !filepath.IsAbs(socket) || len(socket) >= 104 {
		return "", errors.New("--server must be an explicit unix://PATH with an absolute socket path shorter than 104 bytes")
	}
	return socket, nil
}

func ValidateCallTimeout(timeout time.Duration) error {
	if timeout <= 0 || timeout > MaxCallTimeout {
		return errors.New("--call-timeout must be positive and at most 60s")
	}
	return nil
}

func (c Client) command(ctx context.Context, args ...string) ([]byte, error) {
	if !filepath.IsAbs(c.Socket) || len(c.Socket) >= 104 {
		return nil, errors.New("the Rex socket must be an absolute path shorter than 104 bytes")
	}
	timeout := c.CallTimeout
	if timeout == 0 {
		timeout = DefaultCallTimeout
	}
	if err := ValidateCallTimeout(timeout); err != nil {
		return nil, err
	}
	attempts := 1
	if readOnlyCommand(args) {
		attempts += readOnlyRetries
	}
	for attempt := 1; ; attempt++ {
		data, err := c.commandOnce(ctx, timeout, args...)
		if err == nil {
			return data, nil
		}
		if ctx.Err() != nil || errors.Is(err, errOutputLimit) || attempts == 1 {
			return nil, err
		}
		if attempt == attempts {
			return nil, fmt.Errorf("after %d attempts: %w", attempt, err)
		}
		timer := time.NewTimer(readRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

func readOnlyCommand(args []string) bool {
	if len(args) == 2 && args[0] == "whoami" && args[1] == "--json" {
		return true
	}
	if len(args) != 4 || args[0] != "api" || args[1] != "call" {
		return false
	}
	switch args[2] {
	case "server.version", "session.list", "session.view", "session.list_blocks", "com.superlogical.terminal.pwd", "com.superlogical.terminal.process":
		return true
	default:
		return false
	}
}

func (c Client) commandOnce(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	all := append([]string{"--server", "unix://" + c.Socket, "--autostart=false", "--timeout", timeout.String()}, args...)
	cmd := exec.CommandContext(ctx, c.Binary, all...)
	cmd.Env = c.Env
	cmd.WaitDelay = time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.Join(err, out.Close())
	}
	kill := cmd.Cancel
	cmd.Cancel = func() error {
		err := kill()
		// Descendants may still hold the write ends after the child exits.
		// Closing our readers lets both goroutines finish before Wait reaps it.
		_ = out.Close()
		_ = stderr.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	type result struct {
		data []byte
		err  error
	}
	read := func(r io.Reader, ch chan<- result) {
		data, err := io.ReadAll(io.LimitReader(r, maxOutput+1))
		if len(data) > maxOutput {
			err = errOutputLimit
			data = nil
			cancel()
		}
		ch <- result{data, err}
	}
	stdoutCh, stderrCh := make(chan result, 1), make(chan result, 1)
	go read(out, stdoutCh)
	go read(stderr, stderrCh)
	stdout, diagnostic := <-stdoutCh, <-stderrCh
	waitErr := cmd.Wait()
	if err := errors.Join(stdout.err, diagnostic.err, ctx.Err(), waitErr); err != nil {
		return nil, fmt.Errorf("rex %s: %w: %s", strings.Join(args, " "), err, diagnostic.data)
	}
	return stdout.data, nil
}

func (c Client) Call(ctx context.Context, method string, args, result any) error {
	if !validMethod(method) {
		return fmt.Errorf("invalid Rex API method %q", method)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	data, err := c.command(ctx, "api", "call", method, string(encoded))
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode %s: %w", method, err)
	}
	return nil
}

func validMethod(method string) bool {
	if len(method) == 0 || method[0] < 'a' || method[0] > 'z' {
		return false
	}
	for i := 1; i < len(method); i++ {
		b := method[i]
		if b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '.' || b == '_' {
			continue
		}
		return false
	}
	return true
}
