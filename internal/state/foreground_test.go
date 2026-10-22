package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

func TestForegroundRecords(t *testing.T) {
	s := openStore(t)
	live := snapshot.SourceID("block:00000000-89ab-4cde-8fab-0123456789ab")
	absent := snapshot.SourceID("block:11111111-89ab-4cde-8fab-0123456789ab")
	if records, skips, err := s.ForegroundRecords([]snapshot.SourceID{live}); err != nil || len(records) != 0 || len(skips) != 0 {
		t.Fatalf("without a records directory: %v %v %v", records, skips, err)
	}
	dir := filepath.Join(s.dir, foregroundDir)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(id snapshot.SourceID, content string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, strings.TrimPrefix(string(id), "block:"))
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write(live, "  slack-tooy chat \n", 0600)
	records, skips, err := s.ForegroundRecords([]snapshot.SourceID{live, absent})
	if err != nil || len(skips) != 0 || records[live] != "slack-tooy chat" || len(records) != 1 {
		t.Fatalf("valid record: %v %v %v", records, skips, err)
	}
	for name, tc := range map[string]struct {
		content string
		mode    os.FileMode
		reason  string
	}{
		"oversized": {strings.Repeat("x", MaxForegroundRecord+1), 0600, "size limit"},
		"newline":   {"cat <<EOF\nhello", 0600, "control character"},
		"shared":    {"vim", 0644, "mode 0600"},
		"empty":     {"\n", 0600, "1 to 1024 bytes"},
		"invalid":   {"\xff\xfe", 0600, "UTF-8"},
	} {
		write(live, tc.content, tc.mode)
		records, skips, err := s.ForegroundRecords([]snapshot.SourceID{live})
		if err != nil || len(records) != 0 || len(skips) != 1 || skips[0].BlockID != live || !strings.Contains(skips[0].Reason, tc.reason) || len(skips[0].Reason) > 256 {
			t.Errorf("%s: %v %+v %v", name, records, skips, err)
		}
	}
	path := write(live, "vim", 0600)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", path); err != nil {
		t.Fatal(err)
	}
	if _, skips, err := s.ForegroundRecords([]snapshot.SourceID{live}); err != nil || len(skips) != 1 {
		t.Fatalf("symlinked record: %+v %v", skips, err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ForegroundRecords([]snapshot.SourceID{live}); err == nil {
		t.Fatal("shared records directory accepted")
	}
	if _, err := s.PruneForeground(nil); err == nil {
		t.Fatal("prune accepted a shared records directory")
	}
}

func TestPruneForeground(t *testing.T) {
	s := openStore(t)
	if n, err := s.PruneForeground(nil); err != nil || n != 0 {
		t.Fatalf("without a records directory: %d %v", n, err)
	}
	dir := filepath.Join(s.dir, foregroundDir)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	live := snapshot.SourceID("block:00000000-89ab-4cde-8fab-0123456789ab")
	gone := snapshot.SourceID("block:11111111-89ab-4cde-8fab-0123456789ab")
	fresh := snapshot.SourceID("block:22222222-89ab-4cde-8fab-0123456789ab")
	old := time.Now().Add(-2 * foregroundGrace)
	files := map[string]bool{}
	for _, tc := range []struct {
		name string
		aged bool
		keep bool
	}{
		{strings.TrimPrefix(string(live), "block:"), true, true},
		{strings.TrimPrefix(string(gone), "block:"), true, false},
		{strings.TrimPrefix(string(fresh), "block:"), false, true},
		{".tmp-old.1", true, false},
		{".tmp-new.1", false, true},
		{"README", true, true},
	} {
		path := filepath.Join(dir, tc.name)
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if tc.aged {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		files[tc.name] = tc.keep
	}
	n, err := s.PruneForeground(map[snapshot.SourceID]bool{live: true})
	if err != nil || n != 2 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	for name, keep := range files {
		_, err := os.Lstat(filepath.Join(dir, name))
		if exists := err == nil; exists != keep {
			t.Errorf("%s present=%t, want %t", name, exists, keep)
		}
	}
}
