package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/rexbin"
)

// The stand-in refuses any server other than this test's nonexistent socket.
// It never delegates to Rex, starts a server, or invokes launchctl.
const fallbackRexScript = `#!/bin/sh
if [ "$1" != --server ] || [ "$2" != "$RR_EXPECTED_SERVER" ] || [ "$3" != --autostart=false ] || [ "$4" != --timeout ]; then
  printf 'unexpected Rex arguments\n' >&2; exit 2
fi
printf '%s\n' "$0" >> "$MARKER"
if [ -n "$RR_MOVE_EXECUTABLE" ] && [ ! -e "$RR_MOVE_EXECUTABLE.moved" ]; then
  /bin/mv "$RR_MOVE_EXECUTABLE" "$RR_MOVE_EXECUTABLE.moved" || exit 1
fi
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.list) printf '%s\n' '{"sessions":[]}' ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[{"block_id":"block:01234567-89ab-4cde-8fab-0123456789ab","flavor":"com.superlogical.terminal.shell"}]}' ;;
  *) printf 'unexpected method: %s\n' "$8" >&2; exit 1 ;;
esac
`

func TestRexBinaryFallback(t *testing.T) {
	if _, _, err := rexbin.Resolve(""); err == nil {
		t.Fatal("accepted an empty configured Rex executable")
	}
	root, err := os.MkdirTemp("/private/tmp", "rr-fallback-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	apps, pathDir := filepath.Join(root, "Applications"), filepath.Join(root, "path")
	binary, marker := filepath.Join(root, "rexurrect"), filepath.Join(root, "invocations")
	build := exec.CommandContext(t.Context(), "go", "build", "-ldflags", "-X github.com/blacktop/rexurrect/internal/rexbin.applicationsDir="+apps+" -X main.version=1.2.3-test", "-o", binary, "../../cmd/rexurrect")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fallback fixture: %v\n%s", err, out)
	}
	server := "unix://" + filepath.Join(root, "unused.sock")
	fixtureEnv := []string{"PATH=" + pathDir, "HOME=" + root, "MARKER=" + marker, "RR_EXPECTED_SERVER=" + server}
	write := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fallbackRexScript), mode); err != nil {
			t.Fatal(err)
		}
	}
	chmod := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	helper := func(app, name string) string {
		return filepath.Join(apps, app, "Contents", "Helpers", name, "Contents", "MacOS", "rex")
	}
	configured := filepath.Join(root, ".local", "bin", "rex")
	oldHelper := helper("Rex Beta.app", "Rex Server.app")
	newHelper := helper("Rex Beta.app", "Rex Server.rex")
	laterHelper := helper("Rex Beta.app", "Z Later.app")
	stableHelper := helper("Rex.app", "Rex Server.rex")
	pathHelper := filepath.Join(pathDir, "rex")
	write(helper("Rex Beta.app", "A Not Executable.app"), 0600)
	for _, path := range []string{configured, oldHelper, laterHelper, stableHelper, pathHelper} {
		write(path, 0700)
	}
	const uuid = "01234567-89ab-4cde-8fab-0123456789ab"
	commands := [][]string{
		{"save"}, {"status", "--json"}, {"restore", "--json"},
		{"auto", "--json", "--wait", "1s", "--timeout", "5s"},
		{"receipt", "set", "--session", "session:" + uuid, "--block", "block:" + uuid, "--harness", "codex", "--conversation", uuid},
		{"receipt", "list", "--json"},
		{"receipt", "clear", "--session", "session:" + uuid, "--block", "block:" + uuid},
	}
	// Removing the selected executable after the first RPC must fail the
	// operation. A new fallback here would escape the startup diagnostic.
	for _, substituted := range []bool{false, true} {
		name, want := "midcycle-configured-removed", configured
		if substituted {
			name, want = "midcycle-fallback-removed", oldHelper
		}
		t.Run(name, func(t *testing.T) {
			for _, command := range commands {
				name := command[0]
				if name == "receipt" {
					name += "-" + command[1]
				}
				t.Run(name, func(t *testing.T) {
					dir := filepath.Join(t.TempDir(), "saved")
					seed := exec.CommandContext(t.Context(), binary, "save", "--server", server, "--state-dir", dir, "--rex", configured)
					seed.Env = fixtureEnv
					if out, err := seed.CombinedOutput(); err != nil {
						t.Fatalf("seed: %v\n%s", err, out)
					}
					if substituted {
						if err := os.Rename(configured, configured+".unavailable"); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := os.Rename(configured+".unavailable", configured); err != nil {
								t.Error(err)
							}
						})
					}
					t.Cleanup(func() {
						if err := os.Rename(want+".moved", want); err != nil {
							t.Error(err)
						}
					})
					if err := os.WriteFile(marker, nil, 0600); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
					defer cancel()
					args := append(append([]string(nil), command...), "--server", server, "--state-dir", dir, "--rex", configured)
					cmd := exec.CommandContext(ctx, binary, args...)
					cmd.Env = append(append([]string(nil), fixtureEnv...), "RR_MOVE_EXECUTABLE="+want)
					var stdout, stderr bytes.Buffer
					cmd.Stdout, cmd.Stderr = &stdout, &stderr
					if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), want) {
						t.Fatalf("removed executable: %v\n%s\n%s", err, &stdout, &stderr)
					}
					wantReports := 0
					if substituted {
						wantReports = 1
					}
					if strings.Count(stderr.String(), "substituted") != wantReports || substituted && !strings.Contains(stderr.String(), configured) {
						t.Fatalf("substitution diagnostic: %s", &stderr)
					}
					if command[0] == "auto" {
						var result AutoResult
						if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
							t.Fatalf("auto JSON: %s: %v", &stdout, err)
						}
						if sub := result.RexBinarySubstitution; substituted && (sub == nil || *sub != (rexbin.Substitution{Configured: configured, Resolved: want})) || !substituted && sub != nil {
							t.Fatalf("auto substitution: %+v", sub)
						}
					}
					if calls, err := os.ReadFile(marker); err != nil || string(calls) != want+"\n" {
						t.Fatalf("mid-cycle executable changed: %q: %v", calls, err)
					}
				})
			}
		})
	}
	exercise := func(t *testing.T, configured, want string, substituted bool) {
		t.Helper()
		t.Logf("configured %q -> %q", configured, want)
		dir := filepath.Join(t.TempDir(), "saved")
		for _, command := range commands {
			name := command[0]
			if name == "receipt" {
				name += "-" + command[1]
			}
			t.Run(name, func(t *testing.T) {
				if err := os.WriteFile(marker, nil, 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				args := append(append([]string(nil), command...), "--server", server, "--state-dir", dir, "--rex", configured)
				cmd := exec.CommandContext(ctx, binary, args...)
				cmd.Env = fixtureEnv
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				if want == "" {
					if err == nil {
						t.Fatal("accepted missing Rex executable")
					}
					for _, location := range []string{"no executable Rex CLI found", configured, helper("Rex Beta.app", "*"), helper("Rex.app", "*"), "PATH=" + pathDir} {
						if !strings.Contains(stderr.String(), location) {
							t.Errorf("missing diagnostic %q: %s", location, &stderr)
						}
					}
				} else {
					if err != nil {
						t.Fatalf("command: %v\n%s\n%s", err, &stdout, &stderr)
					}
					if got := strings.Contains(stderr.String(), "substituted"); got != substituted || substituted && (!strings.Contains(stderr.String(), configured) || !strings.Contains(stderr.String(), want)) {
						t.Fatalf("substitution diagnostic: %s", &stderr)
					}
				}
				if command[0] == "auto" {
					var result AutoResult
					if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
						t.Fatalf("auto JSON: %s: %v", &stdout, err)
					}
					if substituted {
						sub := result.RexBinarySubstitution
						if sub == nil || *sub != (rexbin.Substitution{Configured: configured, Resolved: want}) {
							t.Fatalf("auto substitution: %+v", sub)
						}
					} else if result.RexBinarySubstitution != nil {
						t.Fatalf("unexpected auto substitution: %+v", result.RexBinarySubstitution)
					}
					if want == "" && (result.State != "unavailable" || !strings.Contains(result.Reason, "no executable Rex CLI")) {
						t.Fatalf("missing binary summary: %+v", result)
					}
				} else if want != "" && (command[0] == "status" || command[0] == "restore" || name == "receipt-list") && !json.Valid(stdout.Bytes()) {
					t.Fatalf("invalid JSON output: %s", &stdout)
				}
				calls, readErr := os.ReadFile(marker)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if want == "" {
					if len(calls) != 0 {
						t.Fatalf("missing binary invoked %q", calls)
					}
					return
				}
				if len(calls) == 0 {
					t.Fatal("Rex was never invoked")
				}
				for _, called := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
					if called != want {
						t.Fatalf("invoked %q, want %q", called, want)
					}
				}
			})
		}
	}
	t.Run("configured-preferred", func(t *testing.T) { exercise(t, configured, configured, false) })
	t.Run("configured-name-preferred", func(t *testing.T) { exercise(t, "rex", pathHelper, false) })
	chmod(configured, 0600)
	t.Run("nonexecutable", func(t *testing.T) { exercise(t, configured, oldHelper, true) })
	if err := os.Remove(configured); err != nil {
		t.Fatal(err)
	}
	t.Run("missing", func(t *testing.T) { exercise(t, configured, oldHelper, true) })
	if err := os.Symlink(oldHelper, configured); err != nil {
		t.Fatal(err)
	}
	t.Run("existing-symlink-preferred", func(t *testing.T) { exercise(t, configured, configured, false) })
	if err := os.Rename(filepath.Dir(filepath.Dir(filepath.Dir(oldHelper))), filepath.Dir(filepath.Dir(filepath.Dir(newHelper)))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configured); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview.15 preflight must leave a dangling symlink: %v", err)
	}
	t.Run("preview15-dangling-symlink", func(t *testing.T) { exercise(t, configured, newHelper, true) })
	chmod(newHelper, 0600)
	t.Run("next-executable-helper", func(t *testing.T) { exercise(t, configured, laterHelper, true) })
	chmod(laterHelper, 0600)
	t.Run("stable-app", func(t *testing.T) { exercise(t, configured, stableHelper, true) })
	chmod(stableHelper, 0600)
	t.Run("path-fallback", func(t *testing.T) { exercise(t, configured, pathHelper, true) })
	chmod(pathHelper, 0600)
	t.Run("none", func(t *testing.T) { exercise(t, configured, "", false) })
	for _, command := range []string{"version", "--version"} {
		cmd := exec.CommandContext(t.Context(), binary, command)
		cmd.Env = fixtureEnv
		if out, err := cmd.CombinedOutput(); err != nil || string(out) != "rexurrect 1.2.3-test\n" {
			t.Fatalf("%s: %q %v", command, out, err)
		}
	}
}
