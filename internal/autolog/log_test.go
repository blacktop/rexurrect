package autolog

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAutomaticLogBounds(t *testing.T) {
	dir := t.TempDir()
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, OutName)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 2048), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	w, err := Wrap(f, path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "next cycle\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != marker+"next cycle\n" {
		t.Fatalf("truncated log: %q %v", got, err)
	}
	long := strings.Repeat("z", 4096)
	if n, err := io.WriteString(w, long); err != nil || n != len(long) {
		t.Fatalf("long write: %d %v", n, err)
	}
	if got, err := os.ReadFile(path); err != nil || len(got) != 1024 || !bytes.HasPrefix(got, []byte(marker)) {
		t.Fatalf("unbounded log: len=%d %v", len(got), err)
	}
	other, err := os.CreateTemp(dir, "other")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
	})
	if w, err := Wrap(other, path, 1024); err != nil || w != other {
		t.Fatal("unrelated output was wrapped")
	}
}

func TestAutomaticLogIndependentWriters(t *testing.T) {
	const limit = 1024
	dir := t.TempDir()
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, OutName)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 2048), 0600); err != nil {
		t.Fatal(err)
	}
	var writers []io.Writer
	for range 2 {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			t.Fatal(err)
		}
		w, err := Wrap(f, path, limit)
		if err != nil {
			t.Fatal(err)
		}
		writers = append(writers, w)
	}
	for i, w := range writers {
		if _, err := fmt.Fprintf(w, "writer %d\n", i); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != marker+"writer 0\nwriter 1\n" {
		t.Fatalf("stale descriptor offset after truncation: %q %v", got, err)
	}

	// A separately opened descriptor must exclude every bounded writer on
	// this inode, including the stat/truncate/marker/payload sequence.
	lock, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writers[0], strings.Repeat("z", 2048))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("writer bypassed the inode lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != marker+"writer 0\nwriter 1\n" {
		t.Fatalf("locked log changed: %q %v", got, err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not finish after unlocking")
	}

	results := make(chan error, len(writers))
	for i, w := range writers {
		go func() {
			payload := strings.Repeat(strconv.Itoa(i), 600)
			for range 100 {
				if n, err := io.WriteString(w, payload); err != nil || n != len(payload) {
					results <- fmt.Errorf("concurrent write: %d %v", n, err)
					return
				}
				data, err := os.ReadFile(path)
				if err != nil || len(data) > limit || bytes.IndexByte(data, 0) >= 0 {
					results <- fmt.Errorf("concurrent log: len=%d, sparse=%t: %v", len(data), bytes.IndexByte(data, 0) >= 0, err)
					return
				}
			}
			results <- nil
		}()
	}
	for range writers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}
