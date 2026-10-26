package rex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

// The retained preflight is copied verbatim in testdata. Tests replace only
// its cwd-bearing fields so the directories belong to the test.
func writeLiveCWDResponses(t *testing.T, root, pwd, foreground, child string) {
	t.Helper()
	data, err := os.ReadFile("testdata/live-cwd.json")
	if err != nil {
		t.Fatal(err)
	}
	var live struct {
		Process map[string]json.RawMessage `json:"process"`
		PWD     map[string]json.RawMessage `json:"pwd"`
	}
	if err := json.Unmarshal(data, &live); err != nil {
		t.Fatal(err)
	}
	encode := func(v any) json.RawMessage {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for field, cwd := range map[string]string{"foreground": foreground, "child": child} {
		var process map[string]json.RawMessage
		if err := json.Unmarshal(live.Process[field], &process); err != nil {
			t.Fatal(err)
		}
		if cwd == "" {
			delete(process, "cwd")
		} else {
			process["cwd"] = encode(cwd)
		}
		live.Process[field] = encode(process)
	}
	live.PWD["pwd"] = encode(pwd)
	for name, response := range map[string]any{"process.json": live.Process, "pwd.json": live.PWD} {
		if err := os.WriteFile(filepath.Join(root, name), encode(response), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

const cwdRexScript = `
if [ "$1" != --server ] || [ "$2" != unix:///private/tmp/rex-client-test-unused ] || [ "$3" != --autostart=false ]; then exit 2; fi
method=$8
if [ "$6" = whoami ]; then method=whoami; fi
printf '%s\n' "$method" >> "$ROOT/calls"
case "$method" in
  whoami) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  session.list) printf '%s\n' '{"sessions":[{"session_id":"session"}]}' ;;
  session.view)
    label=saved
    if [ -f "$ROOT/label" ]; then label=$(/bin/cat "$ROOT/label"); fi
    printf '{"session_id":"session","revision":1,"label":"%s","active_window_id":"window","windows":[{"window_id":"window","label":"shell","focused_block_id":"block","layers":[{"layer_id":"layer","kind":"tiled","layout":{"block_id":"block"},"blocks":[{"block_id":"block","label":"terminal","flavor":"com.superlogical.terminal.shell"}]}]}]}\n' "$label" ;;
  session.list_blocks) printf '%s\n' '{"blocks":[{"block_id":"block","label":"terminal","flavor":"com.superlogical.terminal.shell","placement":"placed","window_id":"window"}]}' ;;
  com.superlogical.terminal.pwd)
    if [ "$PWD_FAIL" = 1 ]; then printf 'pwd unavailable\n' >&2; exit 1; fi
    /bin/cat "$ROOT/pwd.json" ;;
  com.superlogical.terminal.process) /bin/cat "$ROOT/process.json" ;;
  session.create)
    printf '%s\n' "$9" > "$ROOT/create.json"
    printf '%s\n' "$9" | /usr/bin/sed -n 's/.*"label":"\(rexurrect:[0-9a-f]*\)".*/\1/p' > "$ROOT/label"
    if [ "$LOST_ACK" = 1 ]; then printf 'lost create acknowledgement\n' >&2; exit 1; fi
    printf '%s\n' '{"session_id":"session","initial_windows":[{"window_id":"window","block_ids":["block"]}]}' ;;
  session.set_label) printf 'saved\n' > "$ROOT/label"; printf '{}\n' ;;
  session.focus_block|session.zoom_block|session.focus_window) printf '{}\n' ;;
  *) printf 'unexpected method: %s\n' "$method" >&2; exit 1 ;;
esac
`

func TestTerminalCWDLiveShapes(t *testing.T) {
	root, err := snapshot.CanonicalCWD(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"OSC 7", "foreground", "child"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	pwdDir, foreground, child := filepath.Join(root, "OSC 7"), filepath.Join(root, "foreground"), filepath.Join(root, "child")
	alias, regular := filepath.Join(root, "alias"), filepath.Join(root, "regular")
	if err := os.Symlink(pwdDir, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regular, nil, 0600); err != nil {
		t.Fatal(err)
	}
	fileURL := func(path string) string {
		return (&url.URL{Scheme: "file", Host: "mbp-m5.tail50745.ts.net", Path: path, RawQuery: "ignored", Fragment: "ignored"}).String()
	}
	for _, tc := range []struct {
		name, pwd, foreground, child, want string
		pwdFails                           bool
	}{
		{"live-login-fish", fileURL(pwdDir), foreground, "", pwdDir, false},
		{"symlink", fileURL(alias), foreground, child, pwdDir, false},
		{"no-pwd", "", foreground, child, foreground, false},
		{"wrong-scheme", "https://host" + pwdDir, foreground, child, foreground, false},
		{"relative-pwd", "file:relative", foreground, child, foreground, false},
		{"invalid-url", "file://host/%ZZ", foreground, child, foreground, false},
		{"missing-pwd", fileURL(filepath.Join(root, "missing")), foreground, child, foreground, false},
		{"file-not-directory", fileURL(regular), foreground, child, foreground, false},
		{"pwd-call-fails", "", foreground, child, foreground, true},
		{"relative-foreground", "", "relative", child, child, false},
		{"missing-foreground", "", filepath.Join(root, "missing"), child, child, false},
		{"nothing-absolute", "file:relative", "relative", "also-relative", "", false},
		{"nothing-existing", fileURL(regular), filepath.Join(root, "missing"), regular, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := t.TempDir()
			writeLiveCWDResponses(t, responses, tc.pwd, tc.foreground, tc.child)
			fail := "0"
			if tc.pwdFails {
				fail = "1"
			}
			c := scriptClient(t, cwdRexScript, "ROOT="+responses, "PWD_FAIL="+fail)
			got, err := c.terminalCWD(t.Context(), "session", "block")
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "block block has no absolute existing cwd") {
					t.Fatalf("invalid cwd accepted: %+v %v", got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("captured cwd: %+v, want %q: %v", got, tc.want, err)
			}
			calls, err := os.ReadFile(filepath.Join(responses, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := "com.superlogical.terminal.pwd\n"
			if tc.pwdFails {
				wantCalls += strings.Repeat("com.superlogical.terminal.pwd\n", readOnlyRetries)
			}
			if tc.want != pwdDir {
				wantCalls += "com.superlogical.terminal.process\n"
			}
			if string(calls) != wantCalls {
				t.Fatalf("cwd authority order: %q, want %q", calls, wantCalls)
			}
		})
	}
}

func TestCaptureCWDRetries(t *testing.T) {
	home, err := snapshot.CanonicalCWD(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, tc := range []struct {
		name    string
		readyAt int
		cancel  bool
	}{
		{"becomes-ready", 2, false},
		{"substitutes-home", 0, false},
		{"deadline", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeLiveCWDResponses(t, root, "", "", "")
			ready, err := json.Marshal(map[string]any{"foreground": map[string]string{"cwd": home}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "ready.json"), ready, 0600); err != nil {
				t.Fatal(err)
			}
			script := strings.Replace(cwdRexScript, "com.superlogical.terminal.process) /bin/cat \"$ROOT/process.json\" ;;", `com.superlogical.terminal.process)
    count=0
    if [ -f "$ROOT/count" ]; then count=$(/bin/cat "$ROOT/count"); fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$ROOT/count"
    if [ "$count" = "$READY_AT" ]; then /bin/cat "$ROOT/ready.json"; else /bin/cat "$ROOT/process.json"; fi ;;`, 1)
			c := scriptClient(t, script, "ROOT="+root, fmt.Sprintf("READY_AT=%d", tc.readyAt))
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			saved, report, err := c.Capture(ctx, []snapshot.LiveID{"session"})
			if tc.cancel {
				if !errors.Is(err, context.DeadlineExceeded) || len(report.CWDSubstitutions) != 0 {
					t.Fatalf("cancelled capture substituted home: %+v %v", report, err)
				}
				return
			}
			if err != nil || saved.Sessions[0].Windows[0].Layout.Block.CWD != home {
				t.Fatalf("capture: %+v %v", saved, err)
			}
			wantCount := tc.readyAt
			var want []snapshot.CWDSubstitution
			if wantCount == 0 {
				wantCount = cwdAttempts
				want = []snapshot.CWDSubstitution{{BlockID: "block", CWD: home}}
			}
			count, err := os.ReadFile(filepath.Join(root, "count"))
			if err != nil || strings.TrimSpace(string(count)) != fmt.Sprint(wantCount) || !reflect.DeepEqual(report.CWDSubstitutions, want) {
				t.Fatalf("retry count=%q report=%+v: %v", count, report, err)
			}
			if len(want) != 0 {
				ids := snapshot.IDMap{"session": "session", "window": "window", "layer": "layer", "block": "block"}
				if err := c.verifyRestored(t.Context(), saved.Sessions[0], ids, false); err == nil || !strings.Contains(err.Error(), "still has no observable cwd") {
					t.Fatalf("restore accepted a substituted observation: %v", err)
				}
			}
		})
	}
}

func TestSaveNewPaneWithoutCWDPrivateServer(t *testing.T) {
	s := newIsolatedServer(t)
	home := filepath.Join(s.root, "home")
	t.Setenv("HOME", home)
	// Keep the just-exited pane open. Its absent process cwd reproduces the
	// capture shape of a login prompt without requiring a setuid login shell.
	if _, err := s.client.command(t.Context(), "new", "new-pane", "--cwd", s.root, "--keep-open", "--shell", "none", "--json", "--", "/usr/bin/true"); err != nil {
		t.Fatal(err)
	}
	ids := listSessions(t, s.client)
	if len(ids) != 1 {
		t.Fatalf("new pane sessions: %v", ids)
	}
	view, err := s.client.view(t.Context(), ids[0])
	if err != nil || len(view.Windows) != 1 || len(view.Windows[0].Layers) != 1 {
		t.Fatalf("new pane: %+v %v", view, err)
	}
	block := view.Windows[0].Layers[0].Layout.BlockID
	mustCall(t, s.client, "session.new_window", map[string]any{"session_id": ids[0], "window_label": "shell", "layout": createNode{Block: &createBlock{Flavor: shellFlavor, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		_, err := s.client.terminalCWD(ctx, ids[0], block)
		if errors.Is(err, errNoCWD) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("new private pane still has a cwd: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	want := []snapshot.CWDSubstitution{{BlockID: snapshot.SourceID(block), CWD: home}}
	dir := filepath.Join(s.root, "saved")
	saved, err := s.client.Save(t.Context(), dir, false)
	if err != nil || saved.Generation != 1 || !reflect.DeepEqual(saved.Report.CWDSubstitutions, want) || saved.Snapshot.Sessions[0].Windows[0].Layout.Block.CWD != home {
		t.Fatalf("save new pane: %+v %v", saved, err)
	}
	before := savedFiles(t, dir)
	auto, err := s.client.AutoCycle(t.Context(), dir, DefaultAutoWait, privateCycleTimeout)
	if err != nil || auto.State != "same_instance" || auto.Reason != "unchanged" || auto.SavedGeneration != 0 || !reflect.DeepEqual(auto.CaptureCWDSubstitutions, want) || !reflect.DeepEqual(before, savedFiles(t, dir)) {
		t.Fatalf("auto new pane: %+v %v", auto, err)
	}
}

func TestRestoreMissingCWDReported(t *testing.T) {
	root, err := snapshot.CanonicalCWD(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, missing := filepath.Join(root, "home"), filepath.Join(root, "removed")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(missing, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	writeLiveCWDResponses(t, root, (&url.URL{Scheme: "file", Host: "ignored-host", Path: home}).String(), home, "")
	c := scriptClient(t, cwdRexScript, "ROOT="+root, "LOST_ACK=1")
	dir := filepath.Join(root, "saved")
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	source := snapshot.Session{ID: "source", Label: "saved", ActiveWindow: "source-window", Windows: []snapshot.Window{{ID: "source-window", Label: "shell", LayerID: "source-layer", Focus: "source-block", Layout: snapshot.Node{Block: &snapshot.Block{ID: "source-block", Label: "terminal", CWD: missing}}}}}
	e, err := store.Publish(state.Envelope{CapturedAt: time.Now().UTC(), RexVersion: "version", Commit: "commit", HostID: "host", InstanceID: "old-instance", Snapshot: snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{source}}, Report: &snapshot.Report{}}, false)
	if err := errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	want := []state.CWDSubstitution{{BlockID: "source-block", SavedCWD: missing, RestoredCWD: home}}
	first, err := c.RestoreSaved(t.Context(), dir, 0)
	if err == nil || first.Outcome != "uncertain" || !reflect.DeepEqual(first.CWDSubstitutions, want) {
		t.Fatalf("lost acknowledgement must report substitution: %+v %v", first, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "create.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Windows []struct {
			Layout struct {
				Block struct {
					Options map[string]string `json:"options"`
				} `json:"block"`
			} `json:"layout"`
		} `json:"initial_windows"`
	}
	if err := json.Unmarshal(data, &request); err != nil || len(request.Windows) != 1 || !reflect.DeepEqual(request.Windows[0].Layout.Block.Options, map[string]string{"cwd": home}) {
		t.Fatalf("restore must use default terminal options in home: %s: %v", data, err)
	}
	// The original path reappearing must not change an uncertain create's
	// destination or authorize a second create.
	if err := os.Mkdir(missing, 0700); err != nil {
		t.Fatal(err)
	}
	result, err := c.RestoreSaved(t.Context(), dir, e.Generation)
	if err != nil || result.Outcome != "complete" || len(result.IDMap) != 4 || !reflect.DeepEqual(result.CWDSubstitutions, want) {
		t.Fatalf("recover substituted cwd: %+v %v", result, err)
	}
	before := savedFiles(t, dir)
	again, err := c.RestoreSaved(t.Context(), dir, e.Generation)
	if err != nil || !sameRestore(again, result) || !reflect.DeepEqual(savedFiles(t, dir), before) {
		t.Fatalf("completed substitution changed: %+v %v", again, err)
	}
	status, err := c.Status(t.Context(), dir)
	if err != nil || !sameRestore(status.Journal, result) {
		t.Fatalf("substitution missing from status: %+v %v", status, err)
	}
	auto, err := c.AutoCycle(t.Context(), dir, DefaultAutoWait, DefaultAutoTimeout)
	if err != nil || auto.State != "new_instance" || auto.SavedGeneration != 2 || !reflect.DeepEqual(auto.CWDSubstitutions, want) {
		t.Fatalf("automatic substitution result: %+v %v", auto, err)
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil || strings.Count(string(calls), "session.create\n") != 1 {
		t.Fatalf("uncertain create repeated: %q %v", calls, err)
	}
	store, err = state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.LoadGeneration(e.Generation)
	if err := errors.Join(err, store.Close()); err != nil || !reflect.DeepEqual(original, e) {
		t.Fatalf("source generation changed: %+v %v", original, err)
	}
}
