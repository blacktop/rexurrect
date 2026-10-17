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

func TestSnapshotV1Compatibility(t *testing.T) {
	// Compile the exact released CLI, reader, and writer. Only this test's
	// temporary module and state are touched.
	const base = "8974b2ed246cf95d5f5dd73856560b6e250ed15a"
	const baseProject = "rex-" + "resurrect"
	root := t.TempDir()
	names, err := exec.CommandContext(t.Context(), "git", "ls-tree", "-r", "--full-tree", "--name-only", base, "--", "go.mod", "cmd", "internal").Output()
	if err != nil {
		t.Fatalf("list base sources: %v", err)
	}
	for name := range strings.SplitSeq(strings.TrimSpace(string(names)), "\n") {
		if name != "go.mod" && (!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go")) {
			continue
		}
		data, err := exec.CommandContext(t.Context(), "git", "show", base+":"+name).Output()
		if err != nil {
			t.Fatalf("read base %s: %v", name, err)
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
    "github.com/blacktop/` + baseProject + `/internal/state"
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
	binary := filepath.Join(root, "base-state")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exact base reader: %v\n%s", err, out)
	}
	cli := filepath.Join(root, "base-"+baseProject)
	build = exec.CommandContext(t.Context(), "go", "build", "-o", cli, "./cmd/"+baseProject)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exact base CLI: %v\n%s", err, out)
	}
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
			if v2.Snapshot.Version != 2 || current.Envelope.Snapshot.Version != 2 || current.LastKnownGood == nil || current.LastKnownGood.Snapshot.Version != 1 {
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
