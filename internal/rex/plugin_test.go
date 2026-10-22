package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/receipt"
)

func TestPluginMetadata(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "plugins", "rexurrect")
	read := func(path string, value any) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, value); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	var codex struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		Skills     string `json:"skills"`
		Hooks      string `json:"hooks"`
		Extensions struct {
			OpenAI struct {
				OnboardingSkill string `json:"onboardingSkill"`
			} `json:"com.openai"`
		} `json:"extensions"`
	}
	read(filepath.Join(root, ".codex-plugin", "plugin.json"), &codex)
	if codex.Name != "rexurrect" || codex.Version == "" ||
		codex.Skills != "./skills/" || codex.Hooks != "./hooks/codex.json" ||
		codex.Extensions.OpenAI.OnboardingSkill != "./skills/setup/SKILL.md" {
		t.Fatalf("invalid Codex plugin identity or hook selection: %+v", codex)
	}
	if _, err := os.Lstat(filepath.Join(root, "plugin.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root Agent Plugins manifest would suppress hooks in Codex 0.163.0: %v", err)
	}
	var claude struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Hooks   string `json:"hooks"`
	}
	read(filepath.Join(root, ".claude-plugin", "plugin.json"), &claude)
	if claude.Name != codex.Name || claude.Version != codex.Version || claude.Hooks != "./hooks/claude.json" {
		t.Fatalf("Claude manifest differs from Codex package: %+v", claude)
	}
	for _, hooks := range []string{codex.Hooks, claude.Hooks} {
		if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(hooks, "./"))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "hooks", "hooks.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default hooks file would bypass host-specific selection: %v", err)
	}
	for _, skill := range []string{"setup", "status"} {
		if _, err := os.Stat(filepath.Join(root, "skills", skill, "SKILL.md")); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []string{"rexurrect.sh", "codex.fish", "claude.fish", "omp.fish"} {
		if _, err := os.Stat(filepath.Join(root, "shell", entry)); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "shell", "rexurrect-launch")); err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("rexurrect-launch must be an executable script: %v %v", info, err)
	}
	var claudeMarket struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	read(filepath.Join("..", "..", ".claude-plugin", "marketplace.json"), &claudeMarket)
	if claudeMarket.Name != "rexurrect" || len(claudeMarket.Plugins) != 1 ||
		claudeMarket.Plugins[0].Name != codex.Name ||
		claudeMarket.Plugins[0].Source != "./plugins/rexurrect" {
		t.Fatalf("shared marketplace does not resolve the package: %+v", claudeMarket)
	}
}

type pluginHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type pluginHookGroup struct {
	Matcher string              `json:"matcher"`
	Hooks   []pluginHookCommand `json:"hooks"`
}

func decodePluginHooks(t *testing.T, data []byte) map[string][]pluginHookGroup {
	t.Helper()
	var config struct {
		Hooks map[string][]pluginHookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	return config.Hooks
}

func TestPluginReceiptHooks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pack := filepath.Join(root, "plugin with spaces")
	if err := os.CopyFS(pack, os.DirFS("../../plugins/rexurrect")); err != nil {
		t.Fatal(err)
	}
	configs := make(map[string]map[string][]pluginHookGroup)
	for _, host := range []string{"codex", "claude"} {
		data, err := os.ReadFile(filepath.Join(pack, "hooks", host+".json"))
		if err != nil {
			t.Fatal(err)
		}
		configs[host] = decodePluginHooks(t, data)
	}
	doc, err := os.ReadFile("../../docs/compatibility.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(doc), "#### Codex lifecycle hook\n")
	_, rest, start := strings.Cut(section, "```json\n")
	manual, _, end := strings.Cut(rest, "\n```")
	if !found || !start || !end {
		t.Fatal("documented Codex hook configuration not found")
	}
	configs["manual-codex"] = decodePluginHooks(t, []byte(manual))
	commands := make(map[string]map[string]pluginHookCommand)
	for host, config := range configs {
		commands[host] = make(map[string]pluginHookCommand)
		for _, event := range []string{"SessionStart", "SessionEnd"} {
			groups := config[event]
			if len(groups) != 1 || len(groups[0].Hooks) != 1 {
				t.Fatalf("unexpected %s %s configuration", host, event)
			}
			command := groups[0].Hooks[0]
			slowCodexExit := host != "claude" && event == "SessionEnd" && command.Timeout > 3
			if command.Type != "command" || command.Timeout <= 0 || command.Timeout > 60 || slowCodexExit {
				t.Fatalf("invalid %s %s handler: %+v", host, event, command)
			}
			commands[host][event] = command
		}
		matcher, err := regexp.Compile(config["SessionStart"][0].Matcher)
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range []string{"startup", "resume", "clear", "fork", "compact", "unknown"} {
			want := source != "unknown" && (source != "compact" || host == "claude")
			if matcher.MatchString(source) != want {
				t.Fatalf("%s matcher for %s does not match the supported sources", host, source)
			}
		}
	}

	r := commandReceipt(1, "codex", "codex")
	r.Effort = ""
	c := scriptClient(t, `
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[{"block_id":"`+r.BlockID+`","flavor":"com.superlogical.terminal.shell"}]}' ;;
  *) exit 1 ;;
esac
`)
	bin, dir := filepath.Join(root, "bin"), filepath.Join(root, "saved")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	manualHookDir := filepath.Join(root, ".agents", "hooks")
	if err := os.MkdirAll(manualHookDir, 0700); err != nil {
		t.Fatal(err)
	}
	hook, err := os.ReadFile(filepath.Join(pack, "scripts", "receipt-hook.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manualHookDir, "rexurrect-codex.sh"), hook, 0600); err != nil {
		t.Fatal(err)
	}
	buildReceiptCLI(t, bin)
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"rex": c.Binary, "jq": jq} {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	baseEnv := append(paneEnv(root, bin, "unix://"+c.Socket, dir), "CLAUDE_PLUGIN_ROOT="+pack, "PLUGIN_ROOT="+pack)
	runPayload := func(t *testing.T, host, owner string, payload map[string]string, extra ...string) error {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		event := payload["hook_event_name"]
		command := commands[host][event]
		ctx, cancel := context.WithTimeout(t.Context(), time.Duration(command.Timeout)*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command.Command)
		markers := []string{"RR_HARNESS=" + owner, "RR_HOST=" + strings.TrimPrefix(host, "manual-")}
		cmd.Env = append(append(slices.Clone(baseEnv), markers...), extra...)
		cmd.Stdin = bytes.NewReader(data)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", host, event, err, out)
		}
		return nil
	}
	run := func(t *testing.T, host, owner, event, source, id string, extra ...string) error {
		t.Helper()
		payload := map[string]string{"hook_event_name": event, "session_id": id}
		if event == "SessionStart" {
			payload["source"], payload["model"] = source, r.Model
		}
		return runPayload(t, host, owner, payload, extra...)
	}
	assertReceipts := func(t *testing.T, want ...receipt.Receipt) {
		t.Helper()
		got, err := c.ListReceipts(t.Context(), dir)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("receipts: %+v, want %+v: %v", got, want, err)
		}
	}
	for _, tc := range []struct {
		host, harness, source string
		extra                 []string
	}{
		{"codex", "codex", "startup", nil},
		{"codex", "codex", "resume", nil},
		{"codex", "codex", "clear", nil},
		{"codex", "codex", "fork", nil},
		{"manual-codex", "codex", "startup", nil},
		{"manual-codex", "codex", "compact", nil},
		{"claude", "claude", "startup", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude", "resume", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude", "clear", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude", "compact", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude", "fork", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
	} {
		t.Run(tc.host+"/"+tc.harness+"/"+tc.source, func(t *testing.T) {
			if err := run(t, tc.host, tc.harness, "SessionStart", tc.source, r.Conversation, tc.extra...); err != nil {
				t.Fatal(err)
			}
			want := r
			want.Host, want.Command = strings.TrimPrefix(tc.host, "manual-"), tc.harness
			if tc.host == "claude" {
				want.Effort = "high"
			}
			assertReceipts(t, want)
			if tc.source == "startup" {
				if err := run(t, tc.host, tc.harness, "SessionEnd", "", agentReceipt(2, "codex").Conversation, tc.extra...); err != nil {
					t.Fatal(err)
				}
				assertReceipts(t, want)
			}
			if err := run(t, tc.host, tc.harness, "SessionEnd", "", r.Conversation, tc.extra...); err != nil {
				t.Fatal(err)
			}
			assertReceipts(t)
		})
	}

	outer := commandReceipt(3, "codex", "codex")
	outer.Key = r.Key
	if err := c.SetReceipt(t.Context(), dir, outer); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := run(t, "claude", "claude", "SessionEnd", "", outer.Conversation, "CLAUDECODE=1"); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(filepath.Join(dir, "receipts.json")); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("wrong harness cleared the enclosing receipt: %v", err)
	}
	for _, tc := range []struct {
		name, host, owner, event, source string
		extra                            []string
		wantErr                          bool
	}{
		{"missing-owner", "codex", "", "SessionStart", "startup", nil, false},
		{"missing-host", "codex", "codex", "SessionStart", "startup", []string{"RR_HOST="}, false},
		{"foreign-host", "codex", "codex", "SessionStart", "startup", []string{"RR_HOST=claude"}, false},
		{"claude-marker-under-codex", "claude", "claude", "SessionStart", "startup", []string{"RR_HOST=codex"}, false},
		{"nested-claude", "codex", "codex", "SessionStart", "startup", []string{"CLAUDECODE=1"}, false},
		{"nested-codex", "codex", "codex", "SessionStart", "startup", []string{"CODEX_THREAD_ID=outer"}, false},
		{"claude-under-codex", "claude", "claude", "SessionStart", "startup", []string{"CODEX_THREAD_ID=outer"}, false},
		{"nested-end", "codex", "codex", "SessionEnd", "", []string{"CODEX_THREAD_ID=outer"}, false},
		{"unknown-source", "claude", "claude", "SessionStart", "unknown", nil, false},
		{"missing-block", "codex", "codex", "SessionStart", "startup", []string{"REX_BLOCK="}, false},
		{"oversized-input", "codex", "codex", "SessionStart", strings.Repeat("x", 65537), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(t, tc.host, tc.owner, tc.event, tc.source, r.Conversation, tc.extra...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("hook error = %v, want error %t", err, tc.wantErr)
			}
			after, err := os.ReadFile(filepath.Join(dir, "receipts.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("skipped hook changed the enclosing receipt: %v", err)
			}
		})
	}
	if err := runPayload(t, "claude", "claude", map[string]string{
		"hook_event_name": "SessionStart", "session_id": r.Conversation,
		"source": "startup", "model": r.Model, "agent_id": "child-agent",
	}, "CLAUDECODE=1"); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(filepath.Join(dir, "receipts.json")); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("Claude subagent hook changed the enclosing receipt: %v", err)
	}

	envFile := filepath.Join(root, "claude-tool-environment")
	const existing = "export KEEP_EXISTING='yes'\n"
	if err := os.WriteFile(envFile, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	const marker = "export RR_HARNESS='' RR_HOST=''\n"
	if err := run(t, "claude", "claude", "SessionStart", "resume", outer.Conversation,
		"CLAUDECODE=1", "RR_EFFORT=high", "CLAUDE_ENV_FILE="+envFile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(envFile)
	if err != nil || string(data) != existing+marker {
		t.Fatalf("Claude tool environment changed unexpectedly: %q: %v", data, err)
	}
	if err := run(t, "claude", "claude", "SessionStart", "compact", outer.Conversation,
		"CLAUDECODE=1", "RR_EFFORT=high", "CLAUDE_ENV_FILE="+envFile); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(envFile)
	if err != nil || string(data) != existing+marker {
		t.Fatalf("repeated SessionStart duplicated the tool environment marker: %q: %v", data, err)
	}

	for _, model := range []string{"Claude-Opus-4-5", "claude-opus-4-5[1m]", "gpt-5..4", strings.Repeat("m", 129)} {
		if err := runPayload(t, "codex", "codex", map[string]string{
			"hook_event_name": "SessionStart", "session_id": r.Conversation,
			"source": "startup", "model": model,
		}); err != nil {
			t.Fatal(err)
		}
		plain := r
		plain.Model = ""
		assertReceipts(t, plain)
	}
}

// paneEnv is the environment a Rex pane gives the launcher and hooks: the
// receipt key, the rexurrect endpoint and state directory, and a PATH that
// resolves the fixture executables first.
func paneEnv(home, bin, server, stateDir string) []string {
	r := agentReceipt(1, "")
	return []string{
		"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin",
		"REX_SESSION=" + r.SessionID, "REX_BLOCK=" + r.BlockID,
		"REXURRECT_SERVER=" + server, "REXURRECT_STATE_DIR=" + stateDir,
	}
}

type launchCase struct {
	name, host, command string
	args, env           []string
	agent, clear, want  int
	// owner is the RR_HARNESS the agent must see; empty means a plain launch
	// with blank markers and no cleanup.
	owner         string
	model, effort string
	// wantArgs defaults to args.
	wantArgs []string
}

func (tc launchCase) owned() bool { return tc.owner != "" }

// launchFixture installs fake codex, claude, omp, and rexurrect executables that
// record what the launcher passed them, next to the packaged shell files.
func launchFixture(t *testing.T) (bin string) {
	t.Helper()
	pack, err := filepath.Abs("../../plugins/rexurrect")
	if err != nil {
		t.Fatal(err)
	}
	bin = t.TempDir()
	agent := `#!/bin/sh
[ "${RR_HARNESS:-}" = "$RR_EXPECT_OWNER" ] || exit 91
[ "$RR_ACCOUNT_SENTINEL" = keep-me ] || exit 92
[ "${RR_MODEL:-}" = "$RR_EXPECT_MODEL" ] || exit 93
[ "${RR_EFFORT:-}" = "$RR_EXPECT_EFFORT" ] || exit 94
[ "${RR_HOST:-}" = "$RR_EXPECT_HOST" ] || exit 95
# An owned agent is the launcher's direct child and gets its pid; a plain launch gets none.
if [ -n "${RR_HOST:-}" ]; then [ "${RR_LAUNCHER_PID:-}" = "$PPID" ] || exit 96; else [ -z "${RR_LAUNCHER_PID:-}" ] || exit 96; fi
printf '%s\n' "$@" > "$RR_ARGS_PATH"
exit "$RR_AGENT_STATUS"
`
	clear := `#!/bin/sh
printf '%s\n' "$@" >> "$RR_CLEAR_PATH"
exit "$RR_CLEAR_STATUS"
`
	for name, content := range map[string]string{"codex": agent, "claude": agent, "omp": agent, "rexurrect": clear} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"rexurrect-launch", "rexurrect.sh", "codex.fish", "claude.fish", "omp.fish"} {
		if err := os.Symlink(filepath.Join(pack, "shell", name), filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// checkLaunch runs one shell command through the fixture and checks the
// arguments the agent saw, the exit status, and the cleanup call.
func checkLaunch(t *testing.T, bin string, tc launchCase, path string, args ...string) {
	t.Helper()
	if tc.wantArgs == nil {
		tc.wantArgs = tc.args
	}
	if tc.model == "" {
		tc.model = "default-model"
	}
	if tc.effort == "" {
		tc.effort = "medium"
	}
	host := ""
	if tc.owned() {
		host = tc.host
	}
	argsPath, clearPath := filepath.Join(t.TempDir(), "args"), filepath.Join(t.TempDir(), "clear")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(paneEnv(filepath.Dir(bin), bin, "unix:///private/tmp/rex-plugin-test-unused", filepath.Join(bin, "saved")),
		"XDG_CONFIG_HOME="+bin, "XDG_DATA_HOME="+bin,
		"RR_EXPECT_OWNER="+tc.owner, "RR_EXPECT_HOST="+host,
		"RR_MODEL=default-model", "RR_EFFORT=medium",
		"RR_EXPECT_MODEL="+tc.model, "RR_EXPECT_EFFORT="+tc.effort,
		"RR_ACCOUNT_SENTINEL=keep-me", "RR_ARGS_PATH="+argsPath, "RR_CLEAR_PATH="+clearPath,
		fmt.Sprintf("RR_AGENT_STATUS=%d", tc.agent), fmt.Sprintf("RR_CLEAR_STATUS=%d", tc.clear))
	cmd.Env = append(cmd.Env, tc.env...)
	out, err := cmd.CombinedOutput()
	if err != nil && cmd.ProcessState == nil {
		t.Fatal(err)
	}
	if status := cmd.ProcessState.ExitCode(); status != tc.want {
		t.Fatalf("launcher exit %d, want %d: %s", status, tc.want, out)
	}
	got, err := os.ReadFile(argsPath)
	if err != nil || string(got) != strings.Join(tc.wantArgs, "\n")+"\n" {
		t.Fatalf("launcher changed arguments: %q, want %q: %v", got, tc.wantArgs, err)
	}
	data, err := os.ReadFile(clearPath)
	if !tc.owned() {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plain launch cleared the enclosing receipt: %q: %v", data, err)
		}
		return
	}
	r := agentReceipt(1, "")
	want := strings.Join([]string{
		"receipt", "clear", "--server", "unix:///private/tmp/rex-plugin-test-unused",
		"--state-dir", filepath.Join(bin, "saved"), "--session", r.SessionID,
		"--block", r.BlockID, "--command", tc.command, "",
	}, "\n")
	if err != nil || string(data) != want {
		t.Fatalf("launcher cleanup: %q, want %q: %v", data, want, err)
	}
}

var (
	codexArgs  = []string{"arg with spaces", "-c", `model_reasoning_effort="high"`}
	claudeArgs = []string{"arg with spaces", "--model", "model-1.2", "--effort", "high"}
	ompArgs    = []string{"arg with spaces", "--resume", "01a15c18-8aae-7774-8c25-1eb59ff7f355"}
)

// ownedCodex is a Rex-pane launch of codex: --no-daemon is added and the
// launcher captured the effort from -c model_reasoning_effort.
func ownedCodex(name string, agent, clear, want int, env ...string) launchCase {
	return launchCase{name: name, host: "codex", command: "codex", args: codexArgs, env: env,
		agent: agent, clear: clear, want: want, owner: "codex", effort: "high",
		wantArgs: append([]string{"--no-daemon"}, codexArgs...)}
}

// plainCodex passes the arguments through untouched with blank markers.
func plainCodex(name string, args []string, env ...string) launchCase {
	return launchCase{name: name, host: "codex", command: "codex", args: args, env: env,
		agent: 7, clear: 9, want: 7}
}

func ownedClaude(name string, args []string, model, effort string) launchCase {
	return launchCase{name: name, host: "claude", command: "claude", args: args, owner: "claude",
		model: model, effort: effort}
}

// ownedOmp is a Rex-pane launch of omp: no model or effort handling, and the
// arguments pass through untouched.
func ownedOmp(name string, args []string) launchCase {
	return launchCase{name: name, host: "omp", command: "omp", args: args, owner: "omp"}
}

// plainOmp is a launch that must not own the pane: omp's tool shells carry
// OMPCODE and CLAUDECODE, so a nested launch sees them.
func plainOmp(name string, env ...string) launchCase {
	return launchCase{name: name, host: "omp", command: "omp", args: ompArgs, env: env,
		agent: 7, clear: 9, want: 7}
}

func TestPluginShellHelpers(t *testing.T) {
	t.Parallel()
	bin := launchFixture(t)
	launcher := filepath.Join(bin, "rexurrect-launch")
	cases := []launchCase{
		ownedCodex("success", 0, 0, 0),
		ownedCodex("agent-failed", 7, 0, 7),
		ownedCodex("clear-failed", 0, 9, 9),
		ownedCodex("both-failed", 7, 9, 9),
		{name: "explicit-no-daemon", host: "codex", command: "codex", owner: "codex", effort: "high",
			args: append([]string{"--no-daemon"}, codexArgs...)},
		{name: "literal-no-daemon", host: "codex", command: "codex", owner: "codex", effort: "high",
			args:     append(slices.Clone(codexArgs), "--", "--no-daemon"),
			wantArgs: append(append([]string{"--no-daemon"}, codexArgs...), "--", "--no-daemon")},
		{name: "codex-model-flag", host: "codex", command: "codex", owner: "codex", model: "gpt-x",
			args: []string{"-m", "gpt-x", "prompt"}, wantArgs: []string{"--no-daemon", "-m", "gpt-x", "prompt"}},
		{name: "custom-command", host: "codex", command: "codex-work", owner: "codex-work", effort: "high",
			args: codexArgs, wantArgs: append([]string{"--no-daemon"}, codexArgs...)},
		plainCodex("missing-block", codexArgs, "REX_BLOCK="),
		plainCodex("missing-session", codexArgs, "REX_SESSION="),
		plainCodex("nested-claude", codexArgs, "CLAUDECODE=1"),
		plainCodex("nested-codex", codexArgs, "CODEX_THREAD_ID=outer"),
		plainCodex("preset-owner", codexArgs, "RR_HARNESS=codex"),
		plainCodex("blank-preset-owner", codexArgs, "RR_HARNESS="),
		plainCodex("foreign-host", codexArgs, "RR_HOST=claude"),
		plainCodex("daemon-subcommand", []string{"agents", "--json"}),
		plainCodex("daemon-subcommand-after-options", []string{"-c", "model_reasoning_effort=high", "agents"}),
		plainCodex("daemon-subcommand-remote-control", []string{"--enable", "x", "remote-control", "status"}),
		ownedClaude("claude-success", claudeArgs, "model-1.2", "high"),
		ownedOmp("omp-success", ompArgs),
		plainOmp("omp-nested-omp", "OMPCODE=1"),
		plainOmp("omp-preset-launcher-pid", "RR_LAUNCHER_PID=1"),
		ownedClaude("claude-equals", []string{"--model=model-2", "--effort=low"}, "model-2", "low"),
		ownedClaude("claude-defaults", []string{"prompt"}, "", ""),
		ownedClaude("claude-after-separator", []string{"--", "--model", "ignored"}, "", ""),
		{name: "claude-custom-command", host: "claude", command: "claude-work", owner: "claude-work",
			args: claudeArgs, model: "model-1.2", effort: "high"},
		{name: "claude-nested", host: "claude", command: "claude", args: claudeArgs,
			env: []string{"CLAUDECODE=1"}, agent: 7, clear: 9, want: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkLaunch(t, bin, tc, "/bin/sh", append([]string{launcher, tc.host, tc.command}, tc.args...)...)
		})
	}
	t.Run("usage", func(t *testing.T) {
		t.Parallel()
		for _, args := range [][]string{{}, {"codex"}, {"vim", "vim"}, {"codex", ""}} {
			cmd := exec.CommandContext(t.Context(), "/bin/sh", append([]string{launcher}, args...)...)
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
			if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 2 {
				t.Fatalf("launcher accepted %q: %v", args, err)
			}
		}
	})
	// The shell entry points are one-liners; each installed shell must forward
	// through the launcher unchanged.
	entries := map[string]launchCase{
		"codex":  ownedCodex("codex", 0, 0, 0),
		"claude": ownedClaude("claude", []string{"x", "--model", "model-1.2", "--effort", "high"}, "model-1.2", "high"),
		"omp":    ownedOmp("omp", []string{"x", "--resume", "abc"}),
	}
	invoke := map[string]string{
		"codex":  `codex 'arg with spaces' -c 'model_reasoning_effort="high"'`,
		"claude": `claude x --model model-1.2 --effort high`,
		"omp":    `omp x --resume abc`,
	}
	source := `. "` + filepath.Join(bin, "rexurrect.sh") + `"`
	for _, shell := range []struct {
		name, source string
		args         func(script string) []string
	}{
		{"bash", source, func(script string) []string { return []string{"--noprofile", "--norc", "-c", script} }},
		{"zsh", source, func(script string) []string { return []string{"-f", "-c", script} }},
		{"fish", `source "` + filepath.Join(bin, "codex.fish") + `"; source "` + filepath.Join(bin, "claude.fish") + `"; source "` + filepath.Join(bin, "omp.fish") + `"`,
			func(script string) []string { return []string{"--no-config", "--private", "-c", script} }},
	} {
		path, err := exec.LookPath(shell.name)
		if err != nil {
			t.Logf("%s is not installed; skipping its entry point", shell.name)
			continue
		}
		for host, tc := range entries {
			t.Run(shell.name+"/"+host, func(t *testing.T) {
				t.Parallel()
				checkLaunch(t, bin, tc, path, shell.args(shell.source+"\n"+invoke[host]+"\n")...)
			})
		}
	}
}
