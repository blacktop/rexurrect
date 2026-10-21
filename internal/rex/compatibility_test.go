package rex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// buildBaseTools compiles the exact released CLI and a state writer from the
// given commit's sources into a private module. Only that module and the
// returned temporary root are touched.
func buildBaseTools(t *testing.T, base string) (root, writer, cli string) {
	t.Helper()
	root = t.TempDir()
	names, err := exec.CommandContext(t.Context(), "git", "ls-tree", "-r", "--full-tree", "--name-only", base, "--", "go.mod", "cmd", "internal").Output()
	if err != nil {
		t.Fatalf("list base sources: %v", err)
	}
	// The module path and command directory were renamed after 8974b2e, so
	// read both from the base sources instead of assuming them.
	var module, baseProject string
	for name := range strings.SplitSeq(strings.TrimSpace(string(names)), "\n") {
		if name != "go.mod" && (!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go")) {
			continue
		}
		data, err := exec.CommandContext(t.Context(), "git", "show", base+":"+name).Output()
		if err != nil {
			t.Fatalf("read base %s: %v", name, err)
		}
		if name == "go.mod" {
			module = strings.TrimPrefix(strings.SplitN(string(data), "\n", 2)[0], "module ")
		}
		if dir, rest, ok := strings.Cut(name, "/"); ok && dir == "cmd" {
			baseProject = strings.SplitN(rest, "/", 2)[0]
		}
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	main := `package main
import (
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "` + module + `/internal/state"
)
func run() (err error) {
    s, err := state.Open(os.Args[1])
    if err != nil { return err }
    defer func() { err = errors.Join(err, s.Close()) }()
    var e state.Envelope
    if err = json.NewDecoder(os.Stdin).Decode(&e); err != nil { return err }
    e, err = s.Publish(e, false)
    if err != nil { return err }
    return s.WriteJournal(e, state.NewJournal(e, e.HostID, "instance"))
}
func main() {
    if err := run(); err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
}
`
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(main), 0600); err != nil {
		t.Fatal(err)
	}
	writer = filepath.Join(root, "base-state")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", writer, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exact base reader: %v\n%s", err, out)
	}
	cli = filepath.Join(root, "base-"+baseProject)
	build = exec.CommandContext(t.Context(), "go", "build", "-o", cli, "./cmd/"+baseProject)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exact base CLI: %v\n%s", err, out)
	}
	return root, writer, cli
}

func TestSnapshotV1Compatibility(t *testing.T) {
	// Compile the exact released CLI, reader, and writer. Only this test's
	// temporary module and state are touched.
	const base = "8974b2ed246cf95d5f5dd73856560b6e250ed15a"
	root, binary, cli := buildBaseTools(t, base)
	e := state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "old-instance", Snapshot: snapshot.Snapshot{Version: 1, Sessions: []snapshot.Session{{ID: "source", Label: "old"}}}, Report: &snapshot.Report{}}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "saved")
	write := exec.CommandContext(t.Context(), binary, dir)
	write.Stdin = bytes.NewReader(data)
	if out, err := write.CombinedOutput(); err != nil {
		t.Fatalf("base write: %v\n%s", err, out)
	}
	loaded := loadSaved(t, dir).Envelope
	if loaded.Snapshot.Version != 1 {
		t.Fatalf("base snapshot version: %d", loaded.Snapshot.Version)
	}
	label := filepath.Join(root, "label")
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.create)
    printf '%s' "$9" | /usr/bin/sed -n 's/.*"label":"\([^"]*\)".*/\1/p' > "$LABEL"
    printf '%s\n' '{"session_id":"mapped","windows":[]}' ;;
  api:session.set_label) printf '%s\n' old > "$LABEL"; printf '{}\n' ;;
  api:session.view) printf '{"session_id":"mapped","label":"%s","windows":[]}\n' "$(cat "$LABEL")" ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[]}' ;;
  *) exit 1 ;;
esac
`, "LABEL="+label)
	if out, err := exec.CommandContext(t.Context(), cli, "status", "--server", "unix://"+c.Socket, "--state-dir", dir, "--rex", c.Binary, "--json").CombinedOutput(); err != nil {
		t.Fatalf("base status v1: %v\n%s", err, out)
	}
	if j := journalFor(t, c, dir, loaded); j.FormatVersion != 1 || len(j.Records) != 0 {
		t.Fatalf("base journal: %+v", j)
	}
	result, err := c.RestoreSaved(t.Context(), dir, 0)
	if err != nil || result.Outcome != "complete" || result.IDMap["source"] != "mapped" {
		t.Fatalf("restore v1: %+v %v", result, err)
	}
	if j := journalFor(t, c, dir, loaded); j.Status(loaded).Outcome != "complete" {
		t.Fatalf("v1 journal continuation: %+v", j)
	}
	v1Files := savedFiles(t, dir)
	r := agentReceipt(1, "codex")
	for _, tc := range []struct {
		name            string
		receipt, pruned bool
	}{
		{name: "receipt-free"},
		{name: "receipt-bearing", receipt: true},
		{name: "pruning-report-only", pruned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "saved")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for name, data := range v1Files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			e := loaded
			if tc.receipt {
				block := snapshot.SourceID(r.BlockID)
				e.Snapshot = snapshot.Snapshot{Version: 2, Sessions: []snapshot.Session{{
					ID: snapshot.SourceID(r.SessionID), ActiveWindow: "window",
					Windows: []snapshot.Window{{ID: "window", LayerID: "layer", Focus: block,
						Layout: snapshot.Node{Block: &snapshot.Block{ID: block, CWD: root, AgentResume: &r}}}},
				}}}
			}
			if tc.pruned {
				e.Snapshot.Version = 2
				e.Report = &snapshot.Report{PrunedReceipts: []snapshot.SourceID{snapshot.SourceID(r.BlockID)}}
			}
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			v2, publishErr := store.Publish(e, false)
			if err := errors.Join(publishErr, store.Close()); err != nil {
				t.Fatal(err)
			}
			current := loadSaved(t, dir)
			if v2.Snapshot.Version != snapshot.Version || current.Envelope.Snapshot.Version != snapshot.Version || current.LastKnownGood == nil || current.LastKnownGood.Snapshot.Version != 1 {
				t.Fatalf("version upgrade: %+v", current)
			}
			before := savedFiles(t, dir)
			generation := before[fmt.Sprintf("generation-%020d.json", v2.Generation)]
			if strings.Contains(generation, `"agent_resume":`) != tc.receipt || strings.Contains(generation, `"pruned_receipts":`) != tc.pruned {
				t.Fatalf("wrong persisted generation shape: %s", generation)
			}
			for _, command := range []string{"status", "restore", "auto"} {
				t.Run(command, func(t *testing.T) {
					out, err := exec.CommandContext(t.Context(), cli, command, "--server", "unix://"+c.Socket, "--state-dir", dir, "--rex", c.Binary, "--json").CombinedOutput()
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != 1 || (!strings.Contains(string(out), "unsupported snapshot version") && !strings.Contains(string(out), "unknown field")) {
						t.Errorf("exact base %s did not reject v2: %s %v", command, out, err)
					}
					if !maps.Equal(before, savedFiles(t, dir)) {
						t.Fatal("exact base reader changed state")
					}
				})
			}
		})
	}
	t.Log("exact 8974b2e writer's v1 generation and journal restored; its status, restore, and auto commands reject all v2 generation shapes without changing state")
}

// The released v0.1.0 reader (58c87d7) wrote snapshot version 2 and receipt
// store format 1. Check exactly what it does with state this version writes:
// a fully upgraded state is refused without modification, and the
// transitional state (its own v2 generation next to a format 2 store) keeps
// status working while the store itself is refused.
func TestReleasedReaderCompatibility(t *testing.T) {
	t.Parallel()
	const base = "58c87d78a714f351c4bc5459c42e27fe59921bc7"
	root, writer, cli := buildBaseTools(t, base)
	label := filepath.Join(root, "label")
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.create)
    printf '%s' "$9" | /usr/bin/sed -n 's/.*"label":"\([^"]*\)".*/\1/p' > "$LABEL"
    printf '%s\n' '{"session_id":"mapped","windows":[]}' ;;
  api:session.set_label) printf '%s\n' old > "$LABEL"; printf '{}\n' ;;
  api:session.view) printf '{"session_id":"mapped","label":"%s","windows":[]}\n' "$(cat "$LABEL")" ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[]}' ;;
  *) exit 1 ;;
esac
`, "LABEL="+label)
	modern := commandReceipt(1, "codex", "codex-work")
	run := func(t *testing.T, dir string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), cli, append(args, "--server", "unix://"+c.Socket, "--state-dir", dir, "--rex", c.Binary)...)
		out, err := cmd.CombinedOutput()
		if err != nil && cmd.ProcessState == nil {
			t.Fatal(err)
		}
		return string(out), cmd.ProcessState.ExitCode()
	}
	t.Run("upgraded", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "saved")
		store, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		e := state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "old-instance",
			Snapshot: snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{{ID: "source", Label: "new"}}}, Report: &snapshot.Report{}}
		_, publishErr := store.Publish(e, false)
		if err := errors.Join(publishErr, store.SetReceipt("host", modern), store.Close()); err != nil {
			t.Fatal(err)
		}
		before := savedFiles(t, dir)
		for _, command := range []string{"status", "restore", "auto"} {
			out, status := run(t, dir, command, "--json")
			if status != 1 || !strings.Contains(out, "unsupported snapshot version") {
				t.Errorf("released %s accepted a v3 generation: %d %s", command, status, out)
			}
		}
		// The released reader checks the current generation before the store.
		if out, status := run(t, dir, "receipt", "list", "--json"); status != 1 || !strings.Contains(out, "unsupported snapshot version") {
			t.Errorf("released receipt list accepted upgraded state: %d %s", status, out)
		}
		if !maps.Equal(before, savedFiles(t, dir)) {
			t.Fatal("released reader changed upgraded state")
		}
	})
	t.Run("transitional", func(t *testing.T) {
		// The released writer publishes its own v2 generation; this version's
		// store then adds a format 2 receipt file beside it, which happens when
		// a hook registers before the first v3 save.
		dir := filepath.Join(t.TempDir(), "saved")
		e := state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "old-instance",
			Snapshot: snapshot.Snapshot{Version: 2, Sessions: []snapshot.Session{{ID: "source", Label: "old"}}}, Report: &snapshot.Report{}}
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		write := exec.CommandContext(t.Context(), writer, dir)
		write.Stdin = bytes.NewReader(data)
		if out, err := write.CombinedOutput(); err != nil {
			t.Fatalf("released write: %v\n%s", err, out)
		}
		store, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(store.SetReceipt("host", modern), store.Close()); err != nil {
			t.Fatal(err)
		}
		before := savedFiles(t, dir)
		if out, status := run(t, dir, "status", "--json"); status != 0 {
			t.Errorf("released status rejected its own v2 generation beside a format 2 store: %d %s", status, out)
		}
		// Its strict decoder refuses the new field before any binding check.
		if out, status := run(t, dir, "receipt", "list", "--json"); status != 1 || !strings.Contains(out, `unknown field "host"`) {
			t.Errorf("released receipt list accepted a format 2 store: %d %s", status, out)
		}
		if out, status := run(t, dir, "status", "--json"); status != 0 || !maps.Equal(before, savedFiles(t, dir)) {
			t.Errorf("released status after the store check: %d %s", status, out)
		}
	})
}

// The transitional hazard against a real private server: a receipt-bearing v2
// generation written by the released writer, beside a format 2 store written
// by this version, makes the released restore create the session and type its
// legacy command before it discovers the store it cannot read.
func TestReleasedRestoreTransitionalPrivateServer(t *testing.T) {
	const base = "58c87d78a714f351c4bc5459c42e27fe59921bc7"
	s := newIsolatedServer(t)
	_, _, cli := buildBaseTools(t, base)
	dir := filepath.Join(s.root, "saved")
	released := func(t *testing.T, args ...string) (string, int) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), cli, append(args, "--server", "unix://"+s.client.Socket, "--state-dir", dir, "--rex", s.client.Binary)...)
		cmd.Env = s.client.Env
		out, err := cmd.CombinedOutput()
		if err != nil && cmd.ProcessState == nil {
			t.Fatal(err)
		}
		return string(out), cmd.ProcessState.ExitCode()
	}
	var created createdSession
	mustCall(t, s.client, "session.create", map[string]any{"label": "old", "initial_windows": []createWindow{{Label: "old",
		Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Label: "old", Options: shellOptions{CWD: s.root}}}}}}, &created)
	if len(created.Windows) != 1 || len(created.Windows[0].Blocks) != 1 {
		t.Fatalf("create: %+v", created)
	}
	block := created.Windows[0].Blocks[0]
	waitDefaultTerminal(t, s.client, created.ID, block, s.root)
	// The released CLI registers a legacy receipt and saves its own v2
	// generation and format 1 store.
	legacy := agentReceipt(1, "codex")
	if out, status := released(t, "receipt", "set", "--session", string(created.ID), "--block", string(block),
		"--harness", "codex", "--conversation", legacy.Conversation, "--model", legacy.Model, "--effort", legacy.Effort); status != 0 {
		t.Fatalf("released receipt set: %d %s", status, out)
	}
	if out, status := released(t, "save"); status != 0 {
		t.Fatalf("released save: %d %s", status, out)
	}
	saved := loadSaved(t, dir).Envelope
	if saved.Snapshot.Version != 2 || len(saved.Snapshot.Sessions) != 1 || saved.Snapshot.Sessions[0].Blocks()[0].AgentResume == nil {
		t.Fatalf("released generation: %+v", saved)
	}
	// This version's store then rewrites receipts.json as format 2, as a
	// hook registration would after the upgrade but before the first v3 save.
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(store.SetReceipt(saved.HostID, commandReceipt(2, "codex", "codex-work")), store.Close()); err != nil {
		t.Fatal(err)
	}
	before := savedFiles(t, dir)
	if !strings.Contains(before["receipts.json"], `"format_version":2`) {
		t.Fatalf("store was not rewritten: %s", before["receipts.json"])
	}
	out, status := released(t, "restore", "--json")
	if status != 1 || !strings.Contains(out, `unknown field "host"`) {
		t.Fatalf("released restore outcome: %d %s", status, out)
	}
	sessions, err := s.client.List(t.Context())
	if err != nil || len(sessions) != 2 {
		t.Fatalf("released restore left %d sessions: %v", len(sessions), err)
	}
	var journal string
	for name, content := range savedFiles(t, dir) {
		switch {
		case strings.HasPrefix(name, "restore-") && before[name] != content:
			journal = content
		case before[name] != content:
			t.Fatalf("released restore rewrote %s", name)
		}
	}
	if !strings.Contains(journal, `"outcome":"created"`) || strings.Contains(journal, `"outcome":"complete"`) {
		t.Fatalf("released restore journal: %s", journal)
	}
	t.Log("released v0.1.0 restore created a duplicate session and typed its legacy command before failing on the format 2 store; back up state before upgrading")
}
