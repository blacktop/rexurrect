package rex

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
// and records of blocks that no longer exist are pruned once they are old.
func TestAttachForeground(t *testing.T) {
	store := openState(t)
	dir := store.Dir()
	running := snapshot.SourceID("block:00000000-89ab-4cde-8fab-0123456789ab")
	oversized := snapshot.SourceID("block:11111111-89ab-4cde-8fab-0123456789ab")
	multiline := snapshot.SourceID("block:22222222-89ab-4cde-8fab-0123456789ab")
	gone := snapshot.SourceID("block:33333333-89ab-4cde-8fab-0123456789ab")
	fresh := snapshot.SourceID("block:44444444-89ab-4cde-8fab-0123456789ab")
	detached := snapshot.SourceID("block:55555555-89ab-4cde-8fab-0123456789ab")
	writeForeground(t, dir, running, "slack-tooy chat --channel general\n", 0)
	writeForeground(t, dir, oversized, strings.Repeat("x", snapshot.MaxForeground+1), 0)
	writeForeground(t, dir, multiline, "cat <<EOF\nhello\nEOF", 0)
	gonePath := writeForeground(t, dir, gone, "vim notes.md", 2*time.Minute)
	freshPath := writeForeground(t, dir, fresh, "vim notes.md", 0)
	detachedPath := writeForeground(t, dir, detached, "top", 2*time.Minute)
	e := scrollbackEnvelope(running, oversized, multiline)
	e.Report.Unsupported = []snapshot.Unsupported{{SessionID: e.Snapshot.Sessions[0].ID, Kind: "detached", BlockIDs: []snapshot.SourceID{detached}, Flavor: shellFlavor, Reason: "detached"}}
	if err := attachForeground(store, &e); err != nil {
		t.Fatal(err)
	}
	foreground := make(map[snapshot.SourceID]string)
	for _, session := range e.Snapshot.Sessions {
		for _, b := range session.Blocks() {
			foreground[b.ID] = b.Foreground
		}
	}
	if foreground[running] != "slack-tooy chat --channel general" || foreground[oversized] != "" || foreground[multiline] != "" {
		t.Fatalf("attached commands: %+v", foreground)
	}
	if len(e.Report.ForegroundSkipped) != 2 || e.Report.ForegroundSkipped[0].BlockID != oversized || !strings.Contains(e.Report.ForegroundSkipped[0].Reason, "1024 bytes") ||
		e.Report.ForegroundSkipped[1].BlockID != multiline || !strings.Contains(e.Report.ForegroundSkipped[1].Reason, "control character") {
		t.Fatalf("skips: %+v", e.Report.ForegroundSkipped)
	}
	for path, want := range map[string]bool{gonePath: false, freshPath: true, detachedPath: true} {
		_, err := os.Lstat(path)
		if exists := err == nil; exists != want {
			t.Errorf("%s present=%t, want %t", filepath.Base(path), exists, want)
		}
	}
	// A records directory that is not private degrades every block.
	if err := os.Chmod(filepath.Join(dir, "foreground"), 0755); err != nil {
		t.Fatal(err)
	}
	e = scrollbackEnvelope(running)
	if err := attachForeground(store, &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Report.ForegroundSkipped) != 1 || !strings.Contains(e.Report.ForegroundSkipped[0].Reason, "private directory") || e.Snapshot.Sessions[0].Blocks()[0].Foreground != "" {
		t.Fatalf("shared directory: %+v", e.Report.ForegroundSkipped)
	}
}

// A recorded command is typed without a newline; an agent receipt wins.
func TestLayoutTypesForegroundWithoutNewline(t *testing.T) {
	block := snapshot.SourceID("block:01234567-89ab-4cde-8fab-0123456789ab")
	var leaves []snapshot.SourceID
	node := layout(snapshot.Node{Block: &snapshot.Block{ID: block, CWD: "/", Foreground: "slack-tooy chat"}}, &leaves, nil)
	if got := node.Block.Options.InitialInput; got != "slack-tooy chat" {
		t.Fatalf("typed input %q", got)
	}
	r := commandReceipt(1, "claude", "claude")
	r.BlockID = string(block)
	node = layout(snapshot.Node{Block: &snapshot.Block{ID: block, CWD: "/", Foreground: "slack-tooy chat", AgentResume: &r}}, &leaves, nil)
	if got := node.Block.Options.InitialInput; got != r.InitialInput() || !strings.HasSuffix(got, "\n") || strings.Contains(got, "slack") {
		t.Fatalf("receipt did not take precedence: %q", got)
	}
}

func hookScript(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "plugins", "rexurrect", "shell", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func runHook(t *testing.T, shell string, env []string, script string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), shell, "-c", script)
	cmd.Env = append([]string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The shell hooks write the record privately while a command runs, drop it
// when the command finishes, ignore leading-space commands, and install only
// in the outermost shell of a pane.
func TestForegroundHooks(t *testing.T) {
	block := "block:01234567-89ab-4cde-8fab-0123456789ab"
	for _, tc := range []struct {
		shell, file, record, clear, installed string
	}{
		{"zsh", "foreground.zsh", `_rr_foreground_preexec "$1"`, "_rr_foreground_precmd", "whence -w _rr_foreground_preexec >/dev/null"},
		{"fish", "foreground.fish", `emit fish_preexec "$argv[1]"`, `emit fish_postexec ""`, "functions -q _rr_foreground_preexec"},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			shell, err := exec.LookPath(tc.shell)
			if err != nil {
				t.Skipf("%s not installed", tc.shell)
			}
			dir := t.TempDir()
			env := []string{"REX_BLOCK=" + block, "REX_SESSION=session:01234567-89ab-4cde-8fab-0123456789ab", "REXURRECT_STATE_DIR=" + dir}
			record := filepath.Join(dir, "foreground", strings.TrimPrefix(block, "block:"))
			source := "source " + hookScript(t, tc.file) + "; "
			arg := "$1"
			if tc.shell == "fish" {
				arg = "$argv[1]"
			}
			script := source + "set -- 'slack-tooy chat'; " + tc.record
			if tc.shell == "fish" {
				script = source + "set argv 'slack-tooy chat'; " + tc.record
			}
			if out, err := runHook(t, shell, env, script); err != nil {
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
			if out, err := runHook(t, shell, env, source+tc.clear); err != nil {
				t.Fatalf("clear: %v\n%s", err, out)
			}
			if _, err := os.Lstat(record); !os.IsNotExist(err) {
				t.Fatal("record survived the command finishing")
			}
			leading := strings.Replace(script, "'slack-tooy chat'", "' ls'", 1)
			if out, err := runHook(t, shell, env, leading); err != nil {
				t.Fatalf("leading space: %v\n%s", err, out)
			}
			if _, err := os.Lstat(record); !os.IsNotExist(err) {
				t.Fatal("leading-space command was recorded")
			}
			nested := append(append([]string(nil), env...), "RR_FOREGROUND="+block)
			if out, err := runHook(t, shell, nested, source+tc.installed); err == nil {
				t.Fatalf("hook installed in a nested shell:\n%s", out)
			}
			_ = arg
		})
	}
}

// A pane that was running a command comes back with that command typed at
// its prompt and not executed; a pane with an agent receipt resumes the
// agent instead of typing its recorded command.
func TestForegroundRoundTripPrivateServer(t *testing.T) {
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
	for _, b := range saved.Snapshot.Sessions[0].Blocks() {
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
	if strings.Contains(agentText, "touch") {
		t.Fatalf("receipt pane also typed its recorded command:\n%s", agentText)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the typed command ran")
	}
	if text := waitBlockText(t, c, session, newRunning, typed); strings.Count(text, typed) != 1 {
		t.Fatalf("typed command repeated:\n%s", text)
	}
}

// A block whose command will be typed starts through the clearing login
// chain unless it already replays scrollback or resumes an agent.
func TestTypedStarts(t *testing.T) {
	typed := snapshot.SourceID("block:00000000-89ab-4cde-8fab-0123456789ab")
	replayed := snapshot.SourceID("block:11111111-89ab-4cde-8fab-0123456789ab")
	agent := snapshot.SourceID("block:22222222-89ab-4cde-8fab-0123456789ab")
	plain := snapshot.SourceID("block:33333333-89ab-4cde-8fab-0123456789ab")
	e := scrollbackEnvelope(typed, replayed, agent, plain)
	r := commandReceipt(1, "claude", "claude")
	for _, session := range e.Snapshot.Sessions {
		for _, b := range session.Blocks() {
			switch b.ID {
			case typed, replayed:
				b.Foreground = "vim notes.md"
			case agent:
				b.Foreground, b.AgentResume = "claude", &r
			}
		}
	}
	existing := replayCommand{"replay"}
	commands := typedStarts(e, map[snapshot.SourceID]replayCommand{replayed: existing})
	if len(commands) != 2 || !slices.Equal(commands[replayed], existing) {
		t.Fatalf("commands: %v", commands)
	}
	start := commands[typed]
	if len(start) != 7 || start[4] != typedScript || start[6] == "" || !strings.HasPrefix(typedScript, clearScreen) || !strings.HasSuffix(typedScript, loginChain) {
		t.Fatalf("typed start: %q", start)
	}
	if typedStarts(scrollbackEnvelope(plain), nil) != nil {
		t.Fatal("block without a foreground command got a start command")
	}
}
