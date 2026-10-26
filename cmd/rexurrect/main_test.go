package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/foreground"
	"github.com/blacktop/rexurrect/internal/keychain/keychaintest"
	"github.com/blacktop/rexurrect/internal/rex"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

func TestRequiredFlags(t *testing.T) {
	for _, args := range [][]string{
		{"save"}, {"status"}, {"restore"}, {"auto"},
		{"save", "--server", "unix://relative", "--state-dir", "unused"},
		{"save", "--server", "tcp://localhost:1234", "--state-dir", "unused"},
		{"save", "--server", "unix:///private/tmp/unused"},
		{"status", "--server", "unix:///private/tmp/unused", "--state-dir", "unused", "--allow-empty"},
		{"restore", "--server", "unix:///private/tmp/unused", "--state-dir", "unused", "--generation", "-1"},
		{"auto", "--server", "unix:///private/tmp/unused", "--state-dir", "unused", "--allow-empty"},
	} {
		var out, diagnostic bytes.Buffer
		if err := run(t.Context(), args, &out, &diagnostic); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestCallTimeoutFlags(t *testing.T) {
	for _, command := range []string{"save", "restore", "status", "auto", "agent"} {
		for _, timeout := range []string{"0", "-1s", "60.000000001s"} {
			args := []string{command}
			if command == "agent" {
				args = append(args, "install", "--label", "io.blacktop.rexurrect.test.unit-timeout", "--plist-dir", t.TempDir())
			}
			args = append(args, "--server", "unix:///private/tmp/unused", "--state-dir", filepath.Join(t.TempDir(), "state"), "--rex", "/bin/sh", "--call-timeout", timeout)
			var out, diagnostic bytes.Buffer
			if err := run(t.Context(), args, &out, &diagnostic); err == nil || !strings.Contains(err.Error(), "--call-timeout") {
				t.Errorf("%s %s: %v", command, timeout, err)
			}
		}
	}
}

func TestRestoreJSONGenerationSelection(t *testing.T) {
	root := t.TempDir()
	dir, binary := filepath.Join(root, "state"), filepath.Join(root, "rex")
	if err := os.WriteFile(binary, []byte(`#!/bin/sh
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"new-instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  *) exit 1 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.Publish(state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "saved-instance", Snapshot: snapshot.Snapshot{Version: snapshot.Version}, Report: &snapshot.Report{}}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	base := []string{"restore", "--server", "unix:///private/tmp/unused", "--state-dir", dir, "--rex", binary, "--json"}
	for _, tc := range []struct {
		args []string
		want uint64
	}{{append(append([]string(nil), base...), "--generation", "1"), 1}, {base, 2}} {
		var out, diagnostic bytes.Buffer
		if err := run(t.Context(), tc.args, &out, &diagnostic); err != nil {
			t.Fatal(err)
		}
		var result state.JournalStatus
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Generation != tc.want || result.Outcome != "complete" || result.IDMap == nil || result.InstanceID != "new-instance" {
			t.Fatalf("restore JSON: %s %v", out.String(), err)
		}
	}
	var out, diagnostic bytes.Buffer
	if err := run(t.Context(), append(append([]string(nil), base...), "--generation", "3"), &out, &diagnostic); err == nil {
		t.Fatal("restore of an unpublished generation succeeded")
	}
}

func TestStatusJSON(t *testing.T) {
	root := t.TempDir()
	dir, binary := filepath.Join(root, "state"), filepath.Join(root, "rex")
	if err := os.WriteFile(binary, []byte(`#!/bin/sh
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"new-instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit","api_min_revision":3,"api_revision":7}' ;;
  api:session.list) printf '%s\n' '{"sessions":[]}' ;;
  *) exit 1 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, publishErr := s.Publish(state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "saved-instance", Snapshot: snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{{ID: "source"}}}, Report: &snapshot.Report{}}, false)
	closeErr := s.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("fixture: %v %v", publishErr, closeErr)
	}
	var out, diagnostic bytes.Buffer
	if err := run(t.Context(), []string{"status", "--server", "unix:///private/tmp/unused", "--state-dir", dir, "--rex", binary, "--json"}, &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	var status rex.Status
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.Generation != 1 || status.HostID != "host" || status.InstanceID != "saved-instance" || strings.TrimSpace(diagnostic.String()) != "" {
		t.Fatalf("JSON status: %s %v", out.String(), err)
	}
	if status.RexAPI != (rex.RexAPI{ServerAPIMinRevision: 3, ServerAPIRevision: 7}) || status.Recovery != (snapshot.Recovery{}) {
		t.Fatalf("JSON status recovery fields: %s", out.String())
	}
	if !strings.Contains(out.String(), `"rex_api":{"server_api_min_revision":3,"server_api_revision":7}`) {
		t.Fatalf("JSON status rex_api shape: %s", out.String())
	}
	s, err = state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	j := state.NewJournal(e, e.HostID, "new-instance")
	marker, err := state.NewOperationMarker()
	if err != nil {
		t.Fatal(err)
	}
	var ids snapshot.IDMap
	for _, step := range []string{"intent", "created", "configuring", "complete"} {
		if step != "intent" {
			ids = snapshot.IDMap{"source": "missing-live-session"}
		}
		if err := s.Record(e, &j, "source", marker, step, ids); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	base := []string{"status", "--server", "unix:///private/tmp/unused", "--state-dir", dir, "--rex", binary}
	out.Reset()
	if err := run(t.Context(), append(base, "--json"), &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.Journal.Outcome != "complete_missing" || len(status.Journal.MissingSessions) != 1 || status.Journal.MissingSessions[0] != "missing-live-session" {
		t.Fatalf("missing-session JSON status: %s %v", out.String(), err)
	}
	out.Reset()
	if err := run(t.Context(), base, &out, &diagnostic); err != nil || !strings.Contains(out.String(), "Journal complete_missing") || !strings.Contains(out.String(), "Missing completed session missing-live-session\n") ||
		!strings.Contains(out.String(), "Recovery: 0 agents resume, 0 commands typed, 0 plain shells, 0 inert receipts\nRex API revision 7 (minimum 3)\n") {
		t.Fatalf("missing-session text status: %s %v", out.String(), err)
	}
}

// The human restore output names every block whose scrollback was not
// replayed, and the CLI reaches the keychain only through the configured
// security tool.
func TestRestoreTextPrintsScrollbackSkips(t *testing.T) {
	fake := keychaintest.New(t, rex.ScrollbackKeychainService)
	t.Setenv("REXURRECT_SECURITY_TOOL", fake.Security)
	root := t.TempDir()
	dir, binary := filepath.Join(root, "state"), filepath.Join(root, "rex")
	if err := os.WriteFile(binary, []byte(`#!/bin/sh
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"new-instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  *) exit 1 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	block := snapshot.SourceID("block:01234567-89ab-4cde-8fab-0123456789ab")
	ref := snapshot.ScrollbackRef{KeyID: strings.Repeat("ab", 16), ObjectID: strings.Repeat("cd", 32)}
	e := state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "saved-instance", Report: &snapshot.Report{},
		Snapshot: snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{{ID: "session:01234567-89ab-4cde-8fab-0123456789ab", ActiveWindow: "window",
			Windows: []snapshot.Window{{ID: "window", LayerID: "layer", Focus: block, Layout: snapshot.Node{Block: &snapshot.Block{ID: block, CWD: "/", Scrollback: &ref}}}}}}}}
	_, publishErr := s.Publish(e, false)
	if err := errors.Join(publishErr, s.Close()); err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	if err := run(t.Context(), []string{"restore", "--server", "unix:///private/tmp/unused", "--state-dir", dir, "--rex", binary}, &out, &diagnostic); err == nil {
		t.Fatal("restore succeeded against a Rex stub that cannot create sessions")
	}
	if want := "Block " + string(block) + ": scrollback not replayed (scrollback sidecar unreadable)\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("restore output lacks %q:\n%s", want, out.String())
	}
	if items, err := os.ReadDir(fake.Items); err != nil || len(items) != 1 {
		t.Fatalf("restore did not provision through the substituted security tool: %v %v", items, err)
	}
}

// The hook command prints the embedded recorder for a supported shell, so a
// cask-installed binary needs no checkout.
func TestHookCommand(t *testing.T) {
	for _, shell := range []string{"zsh", "fish"} {
		var out, diagnostic bytes.Buffer
		if err := run(t.Context(), []string{"hook", shell}, &out, &diagnostic); err != nil {
			t.Fatalf("hook %s: %v", shell, err)
		}
		want, err := foreground.Script(shell)
		if err != nil || !bytes.Equal(out.Bytes(), want) || !bytes.Contains(out.Bytes(), []byte("RR_FOREGROUND")) {
			t.Fatalf("hook %s output differs from the embedded script: %v", shell, err)
		}
	}
	for _, args := range [][]string{{"hook"}, {"hook", "bash"}, {"hook", "zsh", "extra"}} {
		var out, diagnostic bytes.Buffer
		if err := run(t.Context(), args, &out, &diagnostic); err == nil || out.Len() != 0 {
			t.Errorf("accepted %q", args)
		}
	}
}

// A server from before the API range fields existed leaves both out, which
// reports as 0 and 0 rather than failing.
func TestStatusRexAPIOmitted(t *testing.T) {
	root := t.TempDir()
	dir, binary := filepath.Join(root, "state"), filepath.Join(root, "rex")
	if err := os.WriteFile(binary, []byte(`#!/bin/sh
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"new-instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  *) exit 1 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, publishErr := s.Publish(state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "saved-instance", Snapshot: snapshot.Snapshot{Version: snapshot.Version}, Report: &snapshot.Report{}}, true)
	if err := errors.Join(publishErr, s.Close()); err != nil {
		t.Fatal(err)
	}
	base := []string{"status", "--server", "unix:///private/tmp/unused", "--state-dir", dir, "--rex", binary}
	var out, diagnostic bytes.Buffer
	if err := run(t.Context(), append(base, "--json"), &out, &diagnostic); err != nil || !strings.Contains(out.String(), `"rex_api":{"server_api_min_revision":0,"server_api_revision":0}`) {
		t.Fatalf("omitted range JSON: %s %v", out.String(), err)
	}
	out.Reset()
	if err := run(t.Context(), base, &out, &diagnostic); err != nil || !strings.Contains(out.String(), "Rex API revision 0 (minimum 0)\n") {
		t.Fatalf("omitted range text: %s %v", out.String(), err)
	}
}
