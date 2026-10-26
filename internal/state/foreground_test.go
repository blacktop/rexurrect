package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

const (
	liveBlockUUID  = "00000000-89ab-4cde-8fab-0123456789ab"
	goneBlockUUID  = "11111111-89ab-4cde-8fab-0123456789ab"
	freshBlockUUID = "22222222-89ab-4cde-8fab-0123456789ab"
)

func TestForegroundRecords(t *testing.T) {
	s := openStore(t)
	live, absent := snapshot.SourceID("block:"+liveBlockUUID), snapshot.SourceID("block:"+goneBlockUUID)
	if records, failed, err := s.ForegroundRecords([]snapshot.SourceID{live}); err != nil || len(records)+len(failed) != 0 {
		t.Fatalf("without a records directory: %v %v %v", records, failed, err)
	}
	dir := filepath.Join(s.dir, foregroundDir)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, liveBlockUUID, []byte("  slack-tooy chat \n"))
	records, failed, err := s.ForegroundRecords([]snapshot.SourceID{live, absent})
	if err != nil || len(failed) != 0 || len(records) != 1 || records[live] != "slack-tooy chat" {
		t.Fatalf("valid record: %v %v %v", records, failed, err)
	}
	// A block ID that is not a UUID never reaches the filesystem, so it can
	// neither read outside the records directory nor be reported.
	writeFile(t, filepath.Dir(s.dir), "outside", []byte("vim"))
	records, failed, err = s.ForegroundRecords([]snapshot.SourceID{"block:../outside", "block:../../outside", live})
	if err != nil || len(failed) != 0 || len(records) != 1 || records[live] != "slack-tooy chat" {
		t.Fatalf("traversal: %v %v %v", records, failed, err)
	}
	path := filepath.Join(dir, liveBlockUUID)
	for name, tc := range map[string]struct {
		content string
		mode    os.FileMode
		reason  string
	}{
		"oversized": {strings.Repeat("x", maxForegroundRecord+1), 0600, "size limit"},
		"multiline": {"cat <<EOF\nhello", 0600, "non-printable"},
		"shared":    {"vim", 0644, "mode 0600"},
	} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(tc.content), tc.mode); err != nil {
			t.Fatal(err)
		}
		records, failed, err := s.ForegroundRecords([]snapshot.SourceID{live})
		if err != nil || len(records) != 0 || failed[live] == nil || !strings.Contains(failed[live].Error(), tc.reason) {
			t.Errorf("%s: %v %v %v", name, records, failed, err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", path); err != nil {
		t.Fatal(err)
	}
	if _, failed, err := s.ForegroundRecords([]snapshot.SourceID{live}); err != nil || failed[live] == nil {
		t.Fatalf("symlinked record: %v %v", failed, err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ForegroundRecords([]snapshot.SourceID{live}); err == nil {
		t.Fatal("shared records directory accepted")
	}
	if err := s.PruneForeground(nil); err == nil {
		t.Fatal("prune accepted a shared records directory")
	}
}

func TestPruneForeground(t *testing.T) {
	s := openStore(t)
	if err := s.PruneForeground(nil); err != nil {
		t.Fatalf("without a records directory: %v", err)
	}
	dir := filepath.Join(s.dir, foregroundDir)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * foregroundGrace)
	for _, tc := range []struct {
		name       string
		aged, keep bool
	}{
		{liveBlockUUID, true, true},
		{goneBlockUUID, true, false},
		{freshBlockUUID, false, true},
		{".tmp-old.1", true, false},
		{".tmp-new.1", false, true},
		{"README", true, true},
	} {
		writeFile(t, dir, tc.name, []byte("x"))
		if tc.aged {
			if err := os.Chtimes(filepath.Join(dir, tc.name), old, old); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			_, err := os.Lstat(filepath.Join(dir, tc.name))
			if exists := err == nil; exists != tc.keep {
				t.Errorf("%s present=%t, want %t", tc.name, exists, tc.keep)
			}
		})
	}
	if err := s.PruneForeground(map[snapshot.SourceID]bool{"block:" + liveBlockUUID: true}); err != nil {
		t.Fatal(err)
	}
}
