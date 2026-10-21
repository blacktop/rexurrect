package rex

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// savedFiles maps every state file, by path relative to dir, to its content,
// and fails on anything that is not a private file (0600) or directory (0700).
func savedFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != dir && info.Mode().Perm() != 0700 {
				t.Fatalf("private directory %s: %v", path, info.Mode())
			}
			return nil
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("private file %s: %v", path, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[name] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func loadSaved(t *testing.T, dir string) state.Current {
	t.Helper()
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.LoadCurrent()
	closeErr := s.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("load current: %v; close: %v", err, closeErr)
	}
	return current
}

func TestSaveStatusPrivateServer(t *testing.T) {
	s := newIsolatedServer(t)
	dirs := make([]string, 4)
	for i, name := range []string{"first", "left", "top", "bottom"} {
		dirs[i] = filepath.Join(s.root, name)
		if err := os.Mkdir(dirs[i], 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(s.root, "alias")
	if err := os.Symlink(dirs[1], alias); err != nil {
		t.Fatal(err)
	}
	leaf := func(label, cwd string) createNode {
		return createNode{Block: &createBlock{Flavor: shellFlavor, Label: label, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: cwd}}}
	}
	nested := createNode{Split: &createSplit{Direction: "horizontal", Ratio: 0.37, Before: leaf("left", alias), After: createNode{Split: &createSplit{Direction: "vertical", Ratio: 0.61, Before: leaf("top", dirs[2]), After: leaf("bottom", dirs[3])}}}}
	var sources []snapshot.LiveID
	var created []createdSession
	for range 2 {
		var result createdSession
		mustCall(t, s.client, "session.create", map[string]any{"label": "same-session-label", "initial_windows": []createWindow{{Label: "first", Layout: leaf("first", dirs[0])}, {Label: "work", Layout: nested}}}, &result)
		if len(result.Windows) != 2 || len(result.Windows[1].Blocks) != 3 {
			t.Fatalf("fixture inventory: %+v", result)
		}
		sources = append(sources, result.ID)
		created = append(created, result)
		mustCall(t, s.client, "session.focus_block", map[string]any{"session_id": result.ID, "block_id": result.Windows[1].Blocks[2]}, nil)
		mustCall(t, s.client, "session.zoom_block", map[string]any{"session_id": result.ID, "block_id": result.Windows[1].Blocks[2], "zoom": true}, nil)
	}
	var floating struct {
		ID snapshot.LiveID `json:"layer_id"`
	}
	mustCall(t, s.client, "session.new_layer", map[string]any{"session_id": sources[0], "window_id": created[0].Windows[0].ID, "bounds": []float64{0.1, 0.1, 0.6, 0.5}, "layout": leaf("floating", dirs[0]), "focus": false}, &floating)
	var detached struct {
		ID snapshot.LiveID `json:"block_id"`
	}
	mustCall(t, s.client, "session.new_block", map[string]any{"session_id": sources[1], "flavor": shellFlavor, "label": "detached", "options": shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: dirs[0]}}, &detached)
	slices.Sort(sources)
	want, report, err := s.client.Capture(t.Context(), sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unsupported) != 2 || floating.ID == "" || detached.ID == "" {
		t.Fatalf("unsupported fixture: %+v", report)
	}
	identity, err := s.client.identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "resurrect")
	first, err := s.client.Save(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.FormatVersion != state.FormatVersion || first.Generation != 1 || first.CapturedAt.IsZero() || first.RexVersion != identity.RexVersion || first.Commit != identity.Commit || first.HostID != identity.HostID || first.InstanceID != identity.InstanceID || !reflect.DeepEqual(first.Snapshot, want) || !reflect.DeepEqual(first.Report, &report) {
		t.Fatalf("first envelope differs: %+v", first)
	}
	current := loadSaved(t, dir)
	if !reflect.DeepEqual(current.Envelope, first) || current.LastKnownGood != nil {
		t.Fatalf("loaded first generation differs: %+v", current)
	}
	initialFiles := savedFiles(t, dir)
	var pointer map[string]uint64
	if err := json.Unmarshal([]byte(initialFiles["current.json"]), &pointer); err != nil || pointer["generation"] != 1 || pointer["last_known_good"] != 0 {
		t.Fatalf("first current pointer: %v %v", pointer, err)
	}
	second, err := s.client.Save(t.Context(), dir, false)
	if err != nil || second.Generation != 2 {
		t.Fatalf("second save: %+v %v", second, err)
	}
	current = loadSaved(t, dir)
	if !reflect.DeepEqual(current.Envelope, second) || current.LastKnownGood == nil || !reflect.DeepEqual(*current.LastKnownGood, first) {
		t.Fatalf("last-known-good differs: %+v", current)
	}
	status, err := s.client.Status(t.Context(), dir)
	if err != nil || status.Generation != 2 || status.LastKnownGood != 1 || status.HostID != identity.HostID || status.InstanceID != identity.InstanceID || status.Sessions != 2 || status.Windows != 4 || status.Blocks != 8 || status.Unsupported != 2 {
		t.Fatalf("status differs: %+v %v", status, err)
	}
	before := savedFiles(t, dir)
	for name, data := range initialFiles {
		if name != "current.json" && before[name] != data {
			t.Fatalf("old generation changed: %s", name)
		}
	}
	for _, id := range sources {
		mustCall(t, s.client, "session.destroy", map[string]any{"session_id": id}, nil)
	}
	if _, err := s.client.Save(t.Context(), dir, false); err == nil || !strings.Contains(err.Error(), "--allow-empty") {
		t.Fatalf("empty server overwrite: %v", err)
	}
	if !reflect.DeepEqual(savedFiles(t, dir), before) {
		t.Fatal("empty guard changed state directory")
	}
	empty, err := s.client.Save(t.Context(), dir, true)
	if err != nil || empty.Generation != 3 || len(empty.Snapshot.Sessions) != 0 {
		t.Fatalf("explicit empty save: %+v %v", empty, err)
	}
	current = loadSaved(t, dir)
	if current.Envelope.Generation != 3 || current.LastKnownGood == nil || !reflect.DeepEqual(*current.LastKnownGood, second) {
		t.Fatalf("explicit empty save lost last good: %+v", current)
	}
	t.Logf("published generations 1, 2, and explicit empty 3; %d sessions, %d windows, %d blocks, %d unsupported", status.Sessions, status.Windows, status.Blocks, status.Unsupported)
}

func TestSaveRetriesStructuralRevision(t *testing.T) {
	root := t.TempDir()
	count, mode := filepath.Join(root, "count"), filepath.Join(root, "mode")
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reset := func(value string) { write(count, "0"); write(mode, value) }
	reset("stable")
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.list) printf '%s\n' '{"sessions":[{"session_id":"session"}]}' ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[]}' ;;
  api:session.view)
    read -r n < "$COUNT"
    n=$((n+1))
    printf '%s\n' "$n" > "$COUNT"
    read -r mode < "$MODE"
    revision=0
    case "$mode" in
      once) if [ "$n" -ge 3 ]; then revision=1; fi ;;
      always) revision=$n ;;
    esac
    printf '{"session_id":"session","revision":%s,"windows":[]}\n' "$revision" ;;
  *) printf '%s\n' 'unexpected call' >&2; exit 1 ;;
esac
`, "COUNT="+count, "MODE="+mode)
	dir := filepath.Join(root, "state")
	if _, err := c.Save(t.Context(), dir, false); err != nil {
		t.Fatal(err)
	}
	reset("once")
	if e, err := c.Save(t.Context(), dir, false); err != nil || e.Generation != 2 {
		t.Fatalf("transient revision change: %+v %v", e, err)
	}
	checkCount := func(want int) {
		t.Helper()
		data, err := os.ReadFile(count)
		if err != nil || strings.TrimSpace(string(data)) != strconv.Itoa(want) {
			t.Fatalf("session.view count: %s %v; want %d", data, err, want)
		}
	}
	checkCount(6)
	before := savedFiles(t, dir)
	reset("always")
	if _, err := c.Save(t.Context(), dir, false); err == nil || !strings.Contains(err.Error(), "unstable Rex capture after 3 attempts") {
		t.Fatalf("persistent revision change: %v", err)
	}
	checkCount(9)
	if !reflect.DeepEqual(savedFiles(t, dir), before) {
		t.Fatal("unstable capture changed current")
	}
}
