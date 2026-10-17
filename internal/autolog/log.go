// Package autolog bounds the two files used by the automatic LaunchAgent.
package autolog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	DefaultMaxBytes int64 = 1 << 20
	MaxBytes        int64 = 64 << 20
	OutName               = "auto.stdout.log"
	ErrName               = "auto.stderr.log"
	marker                = "rexurrect: log truncated\n"
)

func ValidateLimit(limit int64) error {
	if limit < 1024 || limit > MaxBytes {
		return fmt.Errorf("--log-max-bytes must be between 1024 and %d", MaxBytes)
	}
	return nil
}

// Prepare creates private regular log files without changing existing bytes.
// The caller holds the state-directory lock.
func Prepare(dir string) error {
	for _, name := range []string{OutName, ErrName} {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0600) {
			err = errors.New("automatic log must be a regular private file (mode 0600)")
		}
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
	return nil
}

// Wrap leaves terminals, pipes, and other output files untouched. It bounds
// only a descriptor which is already attached to the expected agent log.
func Wrap(w io.Writer, path string, limit int64) (io.Writer, error) {
	if err := ValidateLimit(limit); err != nil {
		return nil, err
	}
	f, ok := w.(*os.File)
	if !ok {
		return w, nil
	}
	actual, err := f.Stat()
	if err != nil {
		return nil, err
	}
	expected, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	if !os.SameFile(actual, expected) {
		return w, nil
	}
	if !actual.Mode().IsRegular() || actual.Mode().Perm() != 0600 {
		return nil, errors.New("automatic log must be a regular private file (mode 0600)")
	}
	return &writer{file: f, path: path, limit: limit}, nil
}

type writer struct {
	file  *os.File
	path  string
	limit int64
}

func (w *writer) Write(p []byte) (n int, err error) {
	// A fresh append descriptor avoids stale launchd offsets and gives each
	// write its own flock, including concurrent calls through the same writer.
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	expected, err := w.file.Stat()
	if err != nil {
		return 0, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			return 0, fmt.Errorf("lock automatic log: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Closing f releases its lock only after the complete append is finished.
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !os.SameFile(expected, info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return 0, errors.New("automatic log is no longer the expected private file")
	}
	want := len(p)
	if info.Size() > w.limit-int64(len(p)) {
		if err := f.Truncate(0); err != nil {
			return 0, err
		}
		if _, err := io.WriteString(f, marker); err != nil {
			return 0, err
		}
		if keep := w.limit - int64(len(marker)); int64(len(p)) > keep {
			p = p[int64(len(p))-keep:]
		}
	}
	n, err = f.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return n, err
	}
	return want, nil
}
