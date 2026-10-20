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
	var portable struct {
		Schema     string `json:"$schema"`
		Name       string `json:"name"`
		Version    string `json:"version"`
		Extensions struct {
			OpenAI struct {
				Hooks           string `json:"hooks"`
				OnboardingSkill string `json:"onboardingSkill"`
			} `json:"com.openai"`
		} `json:"extensions"`
	}
	read(filepath.Join(root, "plugin.json"), &portable)
	if portable.Schema != "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json" ||
		portable.Name != "rexurrect" || portable.Version == "" ||
		portable.Extensions.OpenAI.Hooks != "./hooks/codex.json" ||
		portable.Extensions.OpenAI.OnboardingSkill != "./skills/setup/SKILL.md" {
		t.Fatalf("invalid portable plugin identity or Codex hook selection: %+v", portable)
	}
	for _, host := range []string{"codex", "claude"} {
		var overlay struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Hooks   string `json:"hooks"`
		}
		read(filepath.Join(root, "."+host+"-plugin", "plugin.json"), &overlay)
		if overlay.Name != portable.Name || overlay.Version != portable.Version || overlay.Hooks != "./hooks/"+host+".json" {
			t.Fatalf("%s overlay differs from portable package: %+v", host, overlay)
		}
		if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(overlay.Hooks, "./"))); err != nil {
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
	for _, entry := range []string{"codex", "claude"} {
		if _, err := os.Stat(filepath.Join(root, "shell", entry+".fish")); err != nil {
			t.Fatal(err)
		}
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
		claudeMarket.Plugins[0].Name != portable.Name ||
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

	r := agentReceipt(1, "codex")
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
	baseEnv := []string{
		"HOME=" + root, "PATH=" + bin + ":/usr/bin:/bin",
		"CLAUDE_PLUGIN_ROOT=" + pack, "PLUGIN_ROOT=" + pack,
		"REX_SESSION=" + r.SessionID, "REX_BLOCK=" + r.BlockID,
		"REXURRECT_SERVER=unix://" + c.Socket, "REXURRECT_STATE_DIR=" + dir,
	}
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
		cmd.Env = append(append(slices.Clone(baseEnv), "RR_HARNESS="+owner), extra...)
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
		{"codex", "codex-team", "startup", nil},
		{"manual-codex", "codex", "startup", nil},
		{"manual-codex", "codex-team", "startup", nil},
		{"claude", "claude", "startup", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude", "resume", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude", "clear", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude", "compact", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude", "fork", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude-ddb", "startup", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
		{"claude", "claude-team", "startup", []string{"CLAUDECODE=1", "RR_EFFORT=high"}},
	} {
		t.Run(tc.host+"/"+tc.harness+"/"+tc.source, func(t *testing.T) {
			if err := run(t, tc.host, tc.harness, "SessionStart", tc.source, r.Conversation, tc.extra...); err != nil {
				t.Fatal(err)
			}
			want := r
			want.Harness = tc.harness
			if tc.host == "claude" {
				want.Effort = "high"
			}
			assertReceipts(t, want)
			if tc.harness == "claude-team" && want.InitialInput() != "" {
				t.Fatal("plugin made claude-team automatically resumable")
			}
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

	outer := agentReceipt(3, "claude-ddb")
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
		{"foreign-codex-owner", "codex", "claude-ddb", "SessionStart", "startup", nil, false},
		{"foreign-claude-owner", "claude", "codex", "SessionStart", "startup", nil, false},
		{"nested-claude", "codex", "codex", "SessionStart", "startup", []string{"CLAUDECODE=1"}, false},
		{"nested-codex", "codex", "codex", "SessionStart", "startup", []string{"CODEX_THREAD_ID=outer"}, false},
		{"claude-under-codex", "claude", "claude", "SessionStart", "startup", []string{"CODEX_THREAD_ID=outer"}, false},
		{"nested-end", "codex", "codex", "SessionEnd", "", []string{"CODEX_THREAD_ID=outer"}, false},
		{"compact", "codex", "codex", "SessionStart", "compact", nil, false},
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
	if err := runPayload(t, "claude", "claude-ddb", map[string]string{
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
	if err := run(t, "claude", "claude-ddb", "SessionStart", "resume", outer.Conversation, "CLAUDECODE=1", "RR_EFFORT=high", "CLAUDE_ENV_FILE="+envFile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(envFile)
	if err != nil || string(data) != existing+"export RR_HARNESS=''\n" {
		t.Fatalf("Claude tool environment changed unexpectedly: %q: %v", data, err)
	}
	if err := run(t, "claude", "claude-ddb", "SessionStart", "compact", outer.Conversation, "CLAUDECODE=1", "RR_EFFORT=high", "CLAUDE_ENV_FILE="+envFile); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(envFile)
	if err != nil || string(data) != existing+"export RR_HARNESS=''\n" {
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

func TestPluginShellHelpers(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Fatal(err)
	}
	pack, err := filepath.Abs("../../plugins/rexurrect")
	if err != nil {
		t.Fatal(err)
	}
	root, bin := t.TempDir(), t.TempDir()
	agent := `#!/bin/sh
[ "${RR_HARNESS:-}" = "$RR_EXPECT_OWNER" ] || exit 91
[ "$RR_ACCOUNT_SENTINEL" = keep-me ] || exit 92
[ "${RR_MODEL:-}" = "$RR_EXPECT_MODEL" ] || exit 93
[ "${RR_EFFORT:-}" = "$RR_EXPECT_EFFORT" ] || exit 94
printf '%s\n' "$@" > "$RR_ARGS_PATH"
exit "$RR_AGENT_STATUS"
`
	clear := `#!/bin/sh
printf '%s\n' "$@" >> "$RR_CLEAR_PATH"
exit "$RR_CLEAR_STATUS"
`
	for name, content := range map[string]string{"codex": agent, "claude": agent, "rexurrect": clear} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	r := agentReceipt(1, "codex")
	for _, host := range []struct{ name, harness string }{{"codex", "codex"}, {"codex", "codex-team"}, {"claude", "claude"}, {"claude", "claude-ddb"}} {
		for _, tc := range []struct {
			name, marker       string
			agent, clear, want int
			clears, nested     bool
		}{
			{"success", "", 0, 0, 0, true, false},
			{"agent-failed", "", 7, 0, 7, true, false},
			{"clear-failed", "", 0, 9, 9, true, false},
			{"both-failed", "", 7, 9, 9, true, false},
			{"missing-block", "REX_BLOCK=", 7, 9, 7, false, false},
			{"nested-claude", "CLAUDECODE=1", 7, 9, 7, false, true},
			{"nested-codex", "CODEX_THREAD_ID=outer", 7, 9, 7, false, true},
			{"nested-helper", "REXURRECT_WRAPPER_ACTIVE=1", 7, 9, 7, false, true},
		} {
			t.Run(host.harness+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				argsPath, clearPath := filepath.Join(t.TempDir(), "args"), filepath.Join(t.TempDir(), "clear")
				owner := host.harness
				if tc.nested {
					owner = ""
				}
				arguments := ` 'arg with spaces' -c 'model_reasoning_effort="high"'`
				wantArgs := "arg with spaces\n-c\nmodel_reasoning_effort=\"high\"\n"
				model, effort := "default-model", "medium"
				if host.name == "claude" {
					arguments = ` 'arg with spaces' --model model-1.2 --effort high`
					wantArgs = "arg with spaces\n--model\nmodel-1.2\n--effort\nhigh\n"
					if !tc.nested {
						model, effort = "model-1.2", "high"
					}
				}
				invoke := "_rexurrect_" + host.name + ` "$RR_TEST_HARNESS"`
				script := `source "$RR_HELPER"` + "\n"
				if host.harness == host.name {
					script += `source "$RR_ENTRY"` + "\n"
					invoke = host.name
				}
				script += invoke + arguments + "\n"
				cmd := exec.CommandContext(ctx, fish, "--no-config", "--private", "-c", script)
				cmd.Env = []string{
					"HOME=" + root, "XDG_CONFIG_HOME=" + root, "XDG_DATA_HOME=" + root,
					"PATH=" + bin + ":/usr/bin:/bin", "RR_HELPER=" + filepath.Join(pack, "shell", "_rexurrect_"+host.name+".fish"),
					"RR_ENTRY=" + filepath.Join(pack, "shell", host.name+".fish"),
					"RR_TEST_HARNESS=" + host.harness, "RR_HARNESS=" + host.harness, "RR_EXPECT_OWNER=" + owner,
					"RR_MODEL=default-model", "RR_EFFORT=medium", "RR_EXPECT_MODEL=" + model, "RR_EXPECT_EFFORT=" + effort,
					"RR_ACCOUNT_SENTINEL=keep-me", "RR_ARGS_PATH=" + argsPath, "RR_CLEAR_PATH=" + clearPath,
					"REX_SESSION=" + r.SessionID, "REX_BLOCK=" + r.BlockID,
					"REXURRECT_SERVER=unix:///private/tmp/rex-plugin-test-unused", "REXURRECT_STATE_DIR=" + filepath.Join(root, "saved"),
					fmt.Sprintf("RR_AGENT_STATUS=%d", tc.agent), fmt.Sprintf("RR_CLEAR_STATUS=%d", tc.clear),
				}
				if tc.marker != "" {
					cmd.Env = append(cmd.Env, tc.marker)
				}
				out, err := cmd.CombinedOutput()
				status := 0
				if err != nil {
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						t.Fatal(err)
					}
					status = exit.ExitCode()
				}
				if status != tc.want {
					t.Fatalf("wrapper exit %d, want %d: %s", status, tc.want, out)
				}
				args, err := os.ReadFile(argsPath)
				if err != nil || string(args) != wantArgs {
					t.Fatalf("wrapper changed arguments: %q: %v", args, err)
				}
				data, err := os.ReadFile(clearPath)
				if !tc.clears {
					if !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("nested wrapper cleared the enclosing receipt: %q: %v", data, err)
					}
					return
				}
				want := strings.Join([]string{
					"receipt", "clear", "--server", "unix:///private/tmp/rex-plugin-test-unused",
					"--state-dir", filepath.Join(root, "saved"), "--session", r.SessionID,
					"--block", r.BlockID, "--harness", host.harness, "",
				}, "\n")
				if err != nil || string(data) != want {
					t.Fatalf("wrapper cleanup: %q, want %q: %v", data, want, err)
				}
			})
		}
	}
	t.Run("claude-team-has-no-launcher", func(t *testing.T) {
		argsPath := filepath.Join(t.TempDir(), "args")
		cmd := exec.CommandContext(t.Context(), fish, "--no-config", "--private", "-c", `source "$RR_HELPER"; _rexurrect_claude claude-team`)
		cmd.Env = []string{"HOME=" + root, "XDG_CONFIG_HOME=" + root, "PATH=" + bin + ":/usr/bin:/bin", "RR_HELPER=" + filepath.Join(pack, "shell", "_rexurrect_claude.fish"), "RR_ARGS_PATH=" + argsPath}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("claude-team launcher was accepted: %v: %s", err, out)
		}
		if _, err := os.Stat(argsPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("claude-team launch reached the executable: %v", err)
		}
	})
}
