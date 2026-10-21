package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// codexProbe drives a private Codex app server over stdio so the test can ask
// the installed loader what it discovers, without a daemon or a network.
type codexProbe struct {
	t       *testing.T
	cancel  context.CancelFunc
	cmd     *exec.Cmd
	stderr  bytes.Buffer
	encoder *json.Encoder
	decoder *json.Decoder
}

func startCodexProbe(t *testing.T, codex, dir string) *codexProbe {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, codex, "--no-daemon", "-c", "features.remote_plugin=false",
		"app-server", "--stdio")
	cmd.Dir = dir
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	// Only PATH comes from the host: the npm launcher needs node, while inherited
	// CODEX_*/OPENAI_* settings would change what the app server does.
	cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + codexHome,
		"PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}
	cmd.WaitDelay = time.Second
	p := &codexProbe{t: t, cancel: cancel, cmd: cmd}
	cmd.Stderr = &p.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.encoder = json.NewEncoder(stdin)
	p.decoder = json.NewDecoder(io.LimitReader(stdout, 4<<20))
	t.Cleanup(p.stop)
	return p
}

// stop is safe to repeat: cancel is idempotent and a second Wait only reports
// that the process was already reaped.
func (p *codexProbe) stop() {
	p.cancel()
	_ = p.cmd.Wait() // The probe deliberately stops its private app server.
}

func (p *codexProbe) notify(method string) {
	p.t.Helper()
	if err := p.encoder.Encode(map[string]string{"method": method}); err != nil {
		p.t.Fatal(err)
	}
}

func (p *codexProbe) request(id int, method string, params, result any) {
	p.t.Helper()
	if err := p.encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		p.t.Fatal(err)
	}
	for {
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := p.decoder.Decode(&response); err != nil {
			p.stop()
			p.t.Fatalf("Codex %s: %v: %s", method, err, p.stderr.String())
		}
		if response.ID != id {
			continue
		}
		if len(response.Error) != 0 {
			p.t.Fatalf("Codex %s: %s", method, response.Error)
		}
		if result != nil {
			if err := json.Unmarshal(response.Result, result); err != nil {
				p.t.Fatal(err)
			}
		}
		return
	}
}

// Ask the installed Codex loader which hooks it discovers. Valid JSON and a
// listed plugin do not establish that Codex will load its lifecycle hooks.
func TestPluginCodexDiscovery(t *testing.T) {
	t.Parallel()
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex CLI is not installed")
	}
	help, err := exec.CommandContext(t.Context(), codex, "--help").Output()
	if err != nil || !strings.Contains(string(help), "--no-daemon") {
		t.Skip("installed Codex predates --no-daemon and plugin/read")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	probe := startCodexProbe(t, codex, repo)
	probe.request(1, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "rexurrect-plugin-test", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, nil)
	probe.notify("initialized")
	var result struct {
		Plugin struct {
			Hooks []struct {
				EventName string `json:"eventName"`
			} `json:"hooks"`
		} `json:"plugin"`
	}
	probe.request(2, "plugin/read", map[string]string{
		"marketplacePath": filepath.Join(repo, ".claude-plugin", "marketplace.json"),
		"pluginName":      "rexurrect",
	}, &result)
	var events []string
	for _, hook := range result.Plugin.Hooks {
		events = append(events, hook.EventName)
	}
	slices.Sort(events)
	if !slices.Equal(events, []string{"sessionEnd", "sessionStart"}) {
		t.Fatalf("Codex discovered hooks %v, want SessionStart and SessionEnd", events)
	}
}
