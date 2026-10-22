package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/keychain"
	"github.com/blacktop/rexurrect/internal/keychain/keychaintest"
	"github.com/blacktop/rexurrect/internal/scrollback"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

func TestScrollbackKeyProvisioning(t *testing.T) {
	s := openStore(t)
	fake := keychaintest.New(t, "io.blacktop.rexurrect.test")
	kc := fake.Store
	stateID, keyID, master, err := s.ScrollbackKey(t.Context(), kc)
	if err != nil || len(master) != keychain.KeySize || stateID == [16]byte{} || keyID == (scrollback.KeyID{}) {
		t.Fatalf("provision: %x %s %d %v", stateID, keyID, len(master), err)
	}
	record, err := s.readScrollbackState()
	if err != nil || record.Status != "ready" || record.KeyID != keyID.String() {
		t.Fatalf("state after provisioning: %+v %v", record, err)
	}
	again, keyAgain, masterAgain, err := s.ScrollbackKey(t.Context(), kc)
	if err != nil || again != stateID || keyAgain != keyID || !bytes.Equal(master, masterAgain) {
		t.Fatalf("second load differs: %v", err)
	}
	// A finalized key that vanished is reported, never regenerated.
	fake.Remove(t, keyID.String())
	if _, _, _, err := s.ScrollbackKey(t.Context(), kc); !errors.Is(err, ErrScrollbackKeyMissing) {
		t.Fatalf("missing finalized key: %v", err)
	}
	if record, err := s.readScrollbackState(); err != nil || record.KeyID != keyID.String() || record.Status != "ready" {
		t.Fatalf("missing key changed the state record: %+v %v", record, err)
	}
	// A pending record whose item exists (crash between Create and the ready
	// write) is finalized with that item.
	record.Status = "pending"
	if err := s.writeScrollbackState(record); err != nil {
		t.Fatal(err)
	}
	if err := kc.Create(t.Context(), keyID.String(), master); err != nil {
		t.Fatal(err)
	}
	if _, _, got, err := s.ScrollbackKey(t.Context(), kc); err != nil || !bytes.Equal(got, master) {
		t.Fatalf("pending recovery: %v", err)
	}
	if record, _ := s.readScrollbackState(); record.Status != "ready" {
		t.Fatalf("pending record not finalized: %+v", record)
	}
	// A pending record with no item provisions the item under the same IDs.
	record.Status = "pending"
	if err := s.writeScrollbackState(record); err != nil {
		t.Fatal(err)
	}
	fake.Remove(t, keyID.String())
	if _, keyNew, fresh, err := s.ScrollbackKey(t.Context(), kc); err != nil || keyNew != keyID || bytes.Equal(fresh, master) {
		t.Fatalf("pending provisioning: %v", err)
	}
	t.Setenv("RR_FAKE_HANG", "1")
	if _, _, _, err := s.ScrollbackKey(t.Context(), kc); !errors.Is(err, keychain.ErrUnavailable) {
		t.Fatalf("hung keychain: %v", err)
	}
	t.Setenv("RR_FAKE_HANG", "")
	for _, bad := range []string{`{"format_version":1,"state_id":"zz","key_id":"` + keyID.String() + `","status":"ready"}`, `{"format_version":2,"state_id":"` + record.StateID + `","key_id":"` + keyID.String() + `","status":"ready"}`, `{"format_version":1,"state_id":"` + record.StateID + `","key_id":"` + keyID.String() + `","status":"lost"}`} {
		if err := os.WriteFile(filepath.Join(s.dir, scrollbackDir, scrollbackStateName), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.ScrollbackKey(t.Context(), kc); err == nil {
			t.Fatalf("accepted state record %s", bad)
		}
	}
}

func testRef(n byte) snapshot.ScrollbackRef {
	return snapshot.ScrollbackRef{KeyID: strings.Repeat("ab", 16), ObjectID: strings.Repeat(string(rune('0'+n)), 64)}
}

// scrollbackEnvelope is a saved layout whose one shell block references ref.
func scrollbackEnvelope(ref snapshot.ScrollbackRef) Envelope {
	e := fixture()
	block := snapshot.SourceID("block:01234567-89ab-4cde-8fab-0123456789ab")
	e.Snapshot.Sessions = []snapshot.Session{{ID: "session:01234567-89ab-4cde-8fab-0123456789ab", ActiveWindow: "window",
		Windows: []snapshot.Window{{ID: "window", LayerID: "layer", Focus: block,
			Layout: snapshot.Node{Block: &snapshot.Block{ID: block, CWD: "/", Scrollback: &ref}}}}}}
	return e
}

func TestScrollbackSidecars(t *testing.T) {
	s := openStore(t)
	ref := testRef(1)
	blob := []byte("RRSBLOB1 ciphertext stand-in")
	if s.HasScrollback(ref) {
		t.Fatal("sidecar exists before write")
	}
	if _, err := s.ReadScrollback(ref); err == nil {
		t.Fatal("read before write")
	}
	if err := s.WriteScrollback(ref, blob); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, scrollbackDir, ref.KeyID, ref.ObjectID+scrollbackSuffix)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("sidecar mode: %v %v", info, err)
	}
	if dirInfo, err := os.Stat(filepath.Dir(path)); err != nil || dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("sidecar directory mode: %v %v", dirInfo, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
	got, err := s.ReadScrollback(ref)
	if err != nil || !bytes.Equal(got, blob) || !s.HasScrollback(ref) {
		t.Fatalf("read back: %q %v", got, err)
	}
	if err := s.WriteScrollback(ref, []byte("different")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadScrollback(ref); !bytes.Equal(got, blob) {
		t.Fatalf("sidecar was overwritten: %q", got)
	}
	for name, bad := range map[string]struct {
		ref  snapshot.ScrollbackRef
		blob []byte
	}{
		"empty blob":    {testRef(2), nil},
		"oversized":     {testRef(2), make([]byte, MaxScrollbackBlob+1)},
		"bad key":       {snapshot.ScrollbackRef{KeyID: "../x", ObjectID: ref.ObjectID}, blob},
		"bad object":    {snapshot.ScrollbackRef{KeyID: ref.KeyID, ObjectID: "../../current"}, blob},
		"uppercase hex": {snapshot.ScrollbackRef{KeyID: strings.ToUpper(ref.KeyID), ObjectID: ref.ObjectID}, blob},
	} {
		if err := s.WriteScrollback(bad.ref, bad.blob); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := os.WriteFile(path, make([]byte, MaxScrollbackBlob+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadScrollback(ref); err == nil {
		t.Fatal("oversized sidecar read")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadScrollback(ref); err == nil {
		t.Fatal("symlinked sidecar read")
	}
}

func TestScrollbackPruneKeepsRetainedAndPinned(t *testing.T) {
	s := openStore(t)
	refs := map[string]snapshot.ScrollbackRef{"current": testRef(1), "previous": testRef(2), "pinned": testRef(3), "orphan": testRef(4), "expired": testRef(5)}
	for _, ref := range refs {
		if err := s.WriteScrollback(ref, []byte("blob "+ref.ObjectID[:4])); err != nil {
			t.Fatal(err)
		}
	}
	tempPath := filepath.Join(s.dir, scrollbackDir, refs["orphan"].KeyID, ".tmp-leftover")
	rootTemp := filepath.Join(s.dir, scrollbackDir, ".tmp-leftover")
	for _, path := range []string{tempPath, rootTemp} {
		if err := os.WriteFile(path, []byte("partial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(ref snapshot.ScrollbackRef) Envelope {
		t.Helper()
		published, err := s.Publish(scrollbackEnvelope(ref), false)
		if err != nil {
			t.Fatal(err)
		}
		return published
	}
	expired := publish(refs["expired"])
	pinned := publish(refs["pinned"])
	publish(refs["previous"])
	publish(refs["current"])
	for _, e := range []Envelope{expired, pinned} {
		if err := s.PinScrollback(e); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-ScrollbackPinExpiry - time.Minute)
	if err := os.Chtimes(filepath.Join(s.dir, scrollbackDir, scrollbackPinsDir, "1"), old, old); err != nil {
		t.Fatal(err)
	}
	current, err := s.LoadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PruneScrollback(current); err != nil {
		t.Fatal(err)
	}
	// The pinned generation's file was pruned by later publications; the pin
	// itself names the sidecars it protects.
	if _, err := os.Lstat(filepath.Join(s.dir, generationName(pinned.Generation))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture: pinned generation file still exists")
	}
	for name, want := range map[string]bool{"current": true, "previous": true, "pinned": true, "orphan": false, "expired": false} {
		if got := s.HasScrollback(refs[name]); got != want {
			t.Errorf("%s sidecar present=%t, want %t", name, got, want)
		}
	}
	for _, path := range []string{tempPath, rootTemp} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary file survived prune: %s", path)
		}
	}
	if _, err := os.Lstat(filepath.Join(s.dir, scrollbackDir, scrollbackPinsDir, "1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired pin survived prune")
	}
	if err := s.PinScrollback(fixture()); err != nil {
		t.Fatalf("pin without references: %v", err)
	}
}

func TestScrollbackSkipValidation(t *testing.T) {
	e := fixture()
	block := snapshot.SourceID("block:01234567-89ab-4cde-8fab-0123456789ab")
	e.Snapshot.Sessions = []snapshot.Session{{ID: "session:01234567-89ab-4cde-8fab-0123456789ab", ActiveWindow: "window",
		Windows: []snapshot.Window{{ID: "window", LayerID: "layer", Focus: block,
			Layout: snapshot.Node{Block: &snapshot.Block{ID: block, CWD: "/"}}}}}}
	e.Report = &snapshot.Report{ScrollbackSkipped: []snapshot.ScrollbackSkip{{BlockID: block, Reason: "alternate screen"}}}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	e.Snapshot.Version = 3
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "snapshot version 4") {
		t.Fatalf("v3 accepted a scrollback skip: %v", err)
	}
	e.Snapshot.Version = snapshot.Version
	for name, skips := range map[string][]snapshot.ScrollbackSkip{
		"unknown block": {{BlockID: "block:ffffffff-89ab-4cde-8fab-0123456789ab", Reason: "x"}},
		"session id":    {{BlockID: "session:01234567-89ab-4cde-8fab-0123456789ab", Reason: "x"}},
		"empty reason":  {{BlockID: block, Reason: ""}},
		"long reason":   {{BlockID: block, Reason: strings.Repeat("r", 257)}},
		"too many":      {{BlockID: block, Reason: "x"}, {BlockID: block, Reason: "y"}, {BlockID: block, Reason: "z"}, {BlockID: block, Reason: "w"}, {BlockID: block, Reason: "v"}},
	} {
		e.Report.ScrollbackSkipped = skips
		if err := e.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestScrollbackRefusesSymlinkedDirectories(t *testing.T) {
	ref := testRef(1)
	blob := []byte("RRSBLOB1 ciphertext stand-in")
	cases := map[string]struct {
		link                             string
		writeFails, pinFails, pruneFails bool
	}{
		"root": {scrollbackDir, true, true, true},
		"key":  {filepath.Join(scrollbackDir, ref.KeyID), true, false, false},
		"pins": {filepath.Join(scrollbackDir, scrollbackPinsDir), false, true, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			outside := t.TempDir()
			victim := filepath.Join(outside, testRef(2).ObjectID+scrollbackSuffix)
			if err := os.WriteFile(victim, []byte("outside the state directory"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.dir, tc.link)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			if err := s.WriteScrollback(ref, blob); (err != nil) != tc.writeFails {
				t.Errorf("WriteScrollback: %v", err)
			}
			if err := s.PinScrollback(scrollbackEnvelope(ref)); (err != nil) != tc.pinFails {
				t.Errorf("PinScrollback: %v", err)
			}
			if name == "root" {
				if _, _, _, err := s.ScrollbackKey(t.Context(), keychaintest.New(t, "io.blacktop.rexurrect.test").Store); err == nil {
					t.Error("ScrollbackKey wrote its state through the symlink")
				}
			}
			if err := s.PruneScrollback(Current{}); (err != nil) != tc.pruneFails {
				t.Errorf("PruneScrollback: %v", err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(victim) {
				t.Fatalf("the directory behind the symlink changed: %v %v", entries, err)
			}
		})
	}
	// A real key directory that is not private is refused as well.
	s := openStore(t)
	if _, err := s.subdirectory(scrollbackDir); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(s.dir, scrollbackDir, ref.KeyID)
	if err := os.Mkdir(shared, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0755); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteScrollback(ref, blob); err == nil {
		t.Error("WriteScrollback accepted a shared key directory")
	}
	if err := s.PruneScrollback(Current{}); err == nil {
		t.Error("PruneScrollback accepted a shared key directory")
	}
}

// State records and pins are written beside their destination, never through
// the state root, so the pending record is durable in scrollback/ before the
// keychain item exists.
func TestScrollbackStateWritesBesideDestination(t *testing.T) {
	s := openStore(t)
	if _, err := s.subdirectory(scrollbackDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(s.dir, 0700); err != nil {
			t.Error(err)
		}
	})
	record := scrollbackState{FormatVersion: 1, StateID: strings.Repeat("cd", 16), KeyID: strings.Repeat("ab", 16), Status: "pending"}
	if err := s.writeScrollbackState(record); err != nil {
		t.Fatalf("pending record went through the state root: %v", err)
	}
	if got, err := s.readScrollbackState(); err != nil || got != record {
		t.Fatalf("read back: %+v %v", got, err)
	}
	if err := s.PinScrollback(scrollbackEnvelope(testRef(1))); err != nil {
		t.Fatalf("pin went through the state root: %v", err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file in the state root: %s", entry.Name())
		}
	}
}
