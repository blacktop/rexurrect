package rex

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/foreground"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

func writeForeground(t *testing.T, dir string, block snapshot.SourceID, command string, age time.Duration) string {
	t.Helper()
	records := filepath.Join(dir, "foreground")
	if err := os.MkdirAll(records, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(records, strings.TrimPrefix(string(block), "block:"))
	if err := os.WriteFile(path, []byte(command), 0600); err != nil {
		t.Fatal(err)
	}
	if age != 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// Records of live blocks are attached, bad records are reported per block,
// the returned live set prunes only records of blocks that no longer exist,
// and a records directory that is not private degrades every block.
func TestAttachForeground(t *testing.T) {
	store := openState(t)
	dir := store.Dir()
	running := snapshot.SourceID("block:00000000-89ab-4cde-8fab-0123456789ab")
	oversized := snapshot.SourceID("block:11111111-89ab-4cde-8fab-0123456789ab")
	gone := snapshot.SourceID("block:22222222-89ab-4cde-8fab-0123456789ab")
	detached := snapshot.SourceID("block:33333333-89ab-4cde-8fab-0123456789ab")
	writeForeground(t, dir, running, "slack-tooy chat --channel general\n", 0)
	writeForeground(t, dir, oversized, strings.Repeat("x", snapshot.MaxForeground+1), 0)
	gonePath := writeForeground(t, dir, gone, "vim notes.md", 2*time.Minute)
	detachedPath := writeForeground(t, dir, detached, "top", 2*time.Minute)
	e := scrollbackEnvelope(running, oversized)
	e.Report.Unsupported = []snapshot.Unsupported{{SessionID: e.Snapshot.Sessions[0].ID, Kind: "detached", BlockIDs: []snapshot.SourceID{detached}, Flavor: shellFlavor, Reason: "detached"}}
	live, prune := attachForeground(store, &e)
	if !prune {
		t.Fatal("private records directory must allow pruning")
	}
	blocks := e.Snapshot.Blocks()
	if blocks[0].Foreground != "slack-tooy chat --channel general" || blocks[1].Foreground != "" {
		t.Fatalf("attached commands: %q %q", blocks[0].Foreground, blocks[1].Foreground)
	}
	if skips := e.Report.ForegroundSkipped; len(skips) != 1 || skips[0].BlockID != oversized || !strings.Contains(skips[0].Reason, "1024 bytes") {
		t.Fatalf("skips: %+v", skips)
	}
	if _, err := os.Lstat(gonePath); err != nil {
		t.Fatal("attaching records pruned before publication")
	}
	if err := store.PruneForeground(live); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(gonePath); !os.IsNotExist(err) {
		t.Fatal("record of a closed pane survived")
	}
	if _, err := os.Lstat(detachedPath); err != nil {
		t.Fatal("record of a live unsupported block was pruned")
	}
	if err := os.Chmod(filepath.Join(dir, "foreground"), 0755); err != nil {
		t.Fatal(err)
	}
	e = scrollbackEnvelope(running)
	if _, prune := attachForeground(store, &e); prune {
		t.Fatal("shared records directory must not be pruned")
	}
	if skips := e.Report.ForegroundSkipped; len(skips) != 1 || !strings.Contains(skips[0].Reason, "private directory") || e.Snapshot.Blocks()[0].Foreground != "" {
		t.Fatalf("shared directory: %+v", skips)
	}
}

// A receipt's resume command runs and wins over a recorded command; a
// recorded command is typed without a newline; any typed input starts
// through the clearing chain unless the block replays scrollback; a plain
// block keeps Rex's default start.
func TestLayoutStartCommands(t *testing.T) {
	block := snapshot.SourceID("block:01234567-89ab-4cde-8fab-0123456789ab")
	typed, replay := replayCommand{"typed-start"}, replayCommand{"replay-start"}
	r := commandReceipt(1, "claude", "claude")
	r.BlockID = string(block)
	options := func(b snapshot.Block, starts startCommands) shellOptions {
		var leaves []snapshot.SourceID
		return layout(snapshot.Node{Block: &b}, &leaves, starts).Block.Options
	}
	starts := startCommands{typed: typed}
	if o := options(snapshot.Block{ID: block, CWD: "/", Foreground: "slack-tooy chat"}, starts); o.InitialInput != "slack-tooy chat" || o.Shell != "none" || !slices.Equal(o.Command, typed) {
		t.Fatalf("foreground block: %+v", o)
	}
	if o := options(snapshot.Block{ID: block, CWD: "/", Foreground: "slack-tooy chat", AgentResume: &r}, starts); o.InitialInput != r.InitialInput() || !strings.HasSuffix(o.InitialInput, "\n") || !slices.Equal(o.Command, typed) {
		t.Fatalf("receipt block: %+v", o)
	}
	if o := options(snapshot.Block{ID: block, CWD: "/", Foreground: "vim"}, startCommands{typed: typed, replay: map[snapshot.SourceID]replayCommand{block: replay}}); !slices.Equal(o.Command, replay) {
		t.Fatalf("replay must win over the typed start: %+v", o)
	}
	if o := options(snapshot.Block{ID: block, CWD: "/"}, starts); o.InitialInput != "" || o.Shell != "" || o.Command != nil {
		t.Fatalf("plain block: %+v", o)
	}
	if o := options(snapshot.Block{ID: block, CWD: "/", Foreground: "vim"}, startCommands{}); o.InitialInput != "vim" || o.Command != nil {
		t.Fatalf("without a typed start the input is still typed on Rex's default start: %+v", o)
	}
	if start := typedStart(); len(start) != 7 || start[4] != typedScript || !strings.HasPrefix(typedScript, clearScreen) || !strings.HasSuffix(typedScript, loginChain) {
		t.Fatalf("typed start: %q", start)
	}
}

func hookScript(t *testing.T, shell string) string {
	t.Helper()
	script, err := foreground.Script(shell)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "foreground."+shell)
	if err := os.WriteFile(path, script, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The shell hooks write the record privately while a command runs, drop it
// when the command finishes, ignore leading-space commands, and install only
// in the outermost shell of a pane.
func TestForegroundHooks(t *testing.T) {
	for _, tc := range []struct {
		shell, record, clear, installed string
	}{
		{"zsh", `_rr_foreground_preexec 'slack-tooy chat'`, "_rr_foreground_precmd", "whence -w _rr_foreground_preexec >/dev/null"},
		{"fish", `emit fish_preexec 'slack-tooy chat'`, `emit fish_postexec ''`, "functions -q _rr_foreground_preexec"},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			shell, err := exec.LookPath(tc.shell)
			if err != nil {
				t.Skipf("%s not installed", tc.shell)
			}
			dir := t.TempDir()
			env := paneEnv(t.TempDir(), t.TempDir(), "unix:///unused", dir)
			block := agentReceipt(1, "").BlockID
			record := filepath.Join(dir, "foreground", strings.TrimPrefix(block, "block:"))
			source := "source " + hookScript(t, tc.shell) + "; "
			run := func(script string, extra ...string) (string, error) {
				cmd := exec.CommandContext(t.Context(), shell, "-c", script)
				cmd.Env = append(slices.Clone(env), extra...)
				out, err := cmd.CombinedOutput()
				return string(out), err
			}
			if out, err := run(source + tc.record); err != nil {
				t.Fatalf("record: %v\n%s", err, out)
			}
			data, err := os.ReadFile(record)
			if err != nil || string(data) != "slack-tooy chat" {
				t.Fatalf("record content %q: %v", data, err)
			}
			if info, _ := os.Stat(record); info.Mode().Perm() != 0600 {
				t.Fatalf("record mode %v", info.Mode())
			}
			if info, _ := os.Stat(filepath.Dir(record)); info.Mode().Perm() != 0700 {
				t.Fatalf("records directory mode %v", info.Mode())
			}
			if entries, _ := os.ReadDir(filepath.Dir(record)); len(entries) != 1 {
				t.Fatalf("temporary file left behind: %v", entries)
			}
			if out, err := run(source + tc.clear); err != nil {
				t.Fatalf("clear: %v\n%s", err, out)
			}
			if _, err := os.Lstat(record); !os.IsNotExist(err) {
				t.Fatal("record survived the command finishing")
			}
			if out, err := run(source + strings.Replace(tc.record, "'slack-tooy chat'", "' ls'", 1)); err != nil {
				t.Fatalf("leading space: %v\n%s", err, out)
			}
			if _, err := os.Lstat(record); !os.IsNotExist(err) {
				t.Fatal("leading-space command was recorded")
			}
			if out, err := run(source+tc.installed, "RR_FOREGROUND="+block); err == nil {
				t.Fatalf("hook installed in a nested shell:\n%s", out)
			}
			if tc.shell == "zsh" {
				// A temporary file the hook cannot open for writing must never
				// be renamed into place as the record.
				stale := `print -n stale > "$_rr_foreground_dir/.tmp-` + strings.TrimPrefix(block, "block:") + `.$$"; chmod 000 "$_rr_foreground_dir"/.tmp-*; ` + tc.record
				if out, err := run(source + stale); err != nil {
					t.Fatalf("stale temporary file: %v\n%s", err, out)
				}
				if _, err := os.Lstat(record); !os.IsNotExist(err) {
					t.Fatal("a failed write published a record")
				}
			}
		})
	}
}

// A pane that was running a command comes back with that command typed at
// its prompt and not executed; a pane with an agent receipt resumes the
// agent instead of typing its recorded command.
func TestForegroundRoundTripPrivateServer(t *testing.T) {
	requireLoginShell(t)
	s := newIsolatedServer(t)
	c := s.client
	dir := filepath.Join(s.root, "saved")
	var created createdSession
	mustCall(t, c, "session.create", map[string]any{"label": "foreground", "initial_windows": []createWindow{
		{Label: "running", Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Label: "running", Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}},
		{Label: "agent", Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Label: "agent", Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}},
	}}, &created)
	running, agent := created.Windows[0].Blocks[0], created.Windows[1].Blocks[0]
	// The pane's cwd is s.root, so the typed line stays short enough not to
	// wrap at the private terminal's width.
	marker := filepath.Join(s.root, "must-not-run")
	typed := "touch must-not-run"
	for _, block := range []snapshot.LiveID{running, agent} {
		writeForeground(t, dir, snapshot.SourceID(block), typed, 0)
	}
	r := commandReceipt(1, "claude", "echo")
	r.SessionID, r.BlockID = string(created.ID), string(agent)
	if err := c.SetReceipt(t.Context(), dir, r); err != nil {
		t.Fatal(err)
	}
	saved, err := c.Save(t.Context(), dir, false)
	if err != nil || len(saved.Report.ForegroundSkipped) != 0 {
		t.Fatalf("save: %+v %v", saved.Report, err)
	}
	for _, b := range saved.Snapshot.Blocks() {
		if b.Foreground != typed || snapshot.LiveID(b.ID) == agent && b.AgentResume == nil {
			t.Fatalf("saved block %s: foreground %q receipt %v", b.ID, b.Foreground, b.AgentResume != nil)
		}
	}
	s.stop(t)
	s.start(t)
	result, err := c.RestoreSaved(t.Context(), dir, 0)
	if err != nil || result.Outcome != "complete" {
		t.Fatalf("restore: %+v %v", result, err)
	}
	session := result.IDMap[saved.Snapshot.Sessions[0].ID]
	newRunning, newAgent := result.IDMap[snapshot.SourceID(running)], result.IDMap[snapshot.SourceID(agent)]
	waitBlockText(t, c, session, newRunning, typed)
	resumed := "--resume " + r.Conversation
	agentText := waitBlockText(t, c, session, newAgent, resumed)
	if strings.Contains(agentText, "touch") || strings.Count(agentText, resumed) != 1 {
		t.Fatalf("receipt pane typed its recorded command or repeated the resume command:\n%s", agentText)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the typed command ran")
	}
	if text := waitBlockText(t, c, session, newRunning, typed); strings.Count(text, typed) != 1 {
		t.Fatalf("typed command repeated:\n%s", text)
	}
}
