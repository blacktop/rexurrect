package launchagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func exitStatus(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code)).Run()
	if err == nil {
		t.Fatal("expected a nonzero exit")
	}
	return err
}

func installed(t *testing.T, c Config) *Agent {
	t.Helper()
	a, err := New(c.Label, filepath.Join(t.TempDir(), "LaunchAgents"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.writePlist(data); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestOwnedConfigRoundTrip(t *testing.T) {
	for _, scrollback := range []bool{false, true} {
		c := testConfig(t)
		c.Scrollback = scrollback
		a := installed(t, c)
		got, err := a.OwnedConfig()
		if err != nil || !reflect.DeepEqual(got, c) {
			t.Fatalf("owned config: %+v %v, want %+v", got, err, c)
		}
	}
	c := testConfig(t)
	a := installed(t, c)
	data, err := os.ReadFile(a.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range [][2]string{{"<string>auto</string>", "<string>save</string>"}, {"<string>--server</string>", "<string>--bogus</string>"}, {"<key>StartInterval</key>", "<key>StartIntervalX</key>"}, {"<string>" + c.Executable + "</string>", "<string>relative</string>"}} {
		if !bytes.Contains(data, []byte(corrupt[0])) {
			t.Fatalf("plist lacks %q", corrupt[0])
		}
		if err := a.writePlist(bytes.Replace(data, []byte(corrupt[0]), []byte(corrupt[1]), 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.OwnedConfig(); err == nil {
			t.Errorf("accepted plist with %q", corrupt[1])
		}
	}
	if _, err := a.OwnedConfig(); err == nil {
		t.Fatal("corrupt plist accepted")
	}
	if err := a.writePlist(bytes.Replace(data, []byte("<string>1</string>"), []byte("<string>0</string>"), 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.OwnedConfig(); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("unowned plist: %v", err)
	}
}

// Wake must consult launchctl only after the parent is proven to be the
// configured server, and must never restart a running job.
func TestWake(t *testing.T) {
	c := testConfig(t)
	const server = 4242
	missing := exitStatus(t, 113)
	type step struct {
		name       string
		parent     int
		installed  bool
		pid        int
		pidErr     error
		printErr   error
		mismatch   bool
		disabled   bool
		parentGone bool
		plistGone  bool
		kick       error
		outcome    string
		reason     string
		wantErr    bool
		wantCalls  []string
	}
	steps := []step{
		{name: "reparented", parent: 1, installed: true, pid: server, outcome: "skipped", reason: "already exited"},
		{name: "no plist", parent: server, outcome: "skipped", reason: "no owned LaunchAgent plist"},
		{name: "other parent", parent: 7, installed: true, pid: server, outcome: "skipped", reason: "parent PID 7 is not the configured Rex server (PID 4242)"},
		{name: "unreachable", parent: server, installed: true, pid: 0, outcome: "skipped", reason: "did not answer"},
		{name: "probe error", parent: server, installed: true, pidErr: errors.New("boom"), wantErr: true},
		{name: "not loaded", parent: server, installed: true, pid: server, printErr: missing, outcome: "skipped", reason: "not loaded", wantCalls: []string{"print"}},
		{name: "different loaded job", parent: server, installed: true, pid: server, mismatch: true, outcome: "skipped", reason: "does not match", wantCalls: []string{"print"}},
		{name: "disabled but loaded", parent: server, installed: true, pid: server, disabled: true, outcome: "skipped", reason: "is disabled", wantCalls: []string{"print", "print-disabled"}},
		{name: "parent exits before dispatch", parent: server, installed: true, pid: server, parentGone: true, outcome: "skipped", reason: "parent process exited", wantCalls: []string{"print", "print-disabled"}},
		{name: "uninstalled while waiting", parent: server, installed: true, pid: server, plistGone: true, outcome: "skipped", reason: "plist changed", wantCalls: []string{"print", "print-disabled"}},
		{name: "unloaded before kick", parent: server, installed: true, pid: server, kick: missing, outcome: "skipped", reason: "not loaded", wantCalls: []string{"print", "print-disabled", "kickstart"}},
		{name: "launchctl error", parent: server, installed: true, pid: server, kick: errors.New("denied"), wantErr: true, wantCalls: []string{"print", "print-disabled", "kickstart"}},
		{name: "woken", parent: server, installed: true, pid: server, outcome: "woken", wantCalls: []string{"print", "print-disabled", "kickstart"}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			var a *Agent
			if s.installed {
				a = installed(t, c)
			} else {
				var err error
				if a, err = New(c.Label, filepath.Join(t.TempDir(), "LaunchAgents")); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			alive := true
			a.launchctl = func(_ context.Context, args ...string) ([]byte, error) {
				target := a.target()
				if args[0] == "print-disabled" {
					target = a.Domain
				}
				if len(args) != 2 || args[1] != target {
					t.Fatalf("wrong launchctl invocation: %q", args)
				}
				calls = append(calls, args[0])
				switch args[0] {
				case "print":
					printed := printedJob(t, a, c)
					if s.mismatch {
						printed = strings.Replace(printed, c.Server, "unix:///private/tmp/other", 1)
					}
					return []byte(printed), s.printErr
				case "print-disabled":
					alive = !s.parentGone
					if s.plistGone {
						if err := os.Remove(a.PlistPath); err != nil {
							t.Fatal(err)
						}
					}
					value := "enabled"
					if s.disabled {
						value = "disabled"
					}
					return []byte("disabled services = {\n\t\"" + a.Label + "\" => " + value + "\n}\n"), nil
				case "kickstart":
					return nil, s.kick
				default:
					t.Fatalf("unexpected launchctl command: %q", args)
					return nil, nil
				}
			}
			probed := false
			got, err := a.Wake(t.Context(), s.parent, func() bool { return alive }, func(_ context.Context, cfg Config) (int, error) {
				probed = true
				if !reflect.DeepEqual(cfg, c) {
					t.Fatalf("probe received %+v, want %+v", cfg, c)
				}
				if len(calls) != 0 {
					t.Fatal("launchctl ran before the identity check")
				}
				return s.pid, s.pidErr
			})
			if (err != nil) != s.wantErr {
				t.Fatalf("error %v, want error %t", err, s.wantErr)
			}
			if !reflect.DeepEqual(calls, s.wantCalls) {
				t.Fatalf("launchctl calls %q, want %q", calls, s.wantCalls)
			}
			if s.wantErr {
				return
			}
			if got.Outcome != s.outcome || !strings.Contains(got.Reason, s.reason) || got.ParentPID != s.parent || got.Label != c.Label || got.Domain != a.Domain {
				t.Fatalf("result %+v, want %s %q", got, s.outcome, s.reason)
			}
			if probed != (s.parent > 1 && s.installed) {
				t.Fatalf("probe ran = %t", probed)
			}
			if encoded, err := json.Marshal(got); err != nil || !bytes.Contains(encoded, []byte(`"outcome":"`+s.outcome+`"`)) {
				t.Fatalf("JSON %s %v", encoded, err)
			}
		})
	}
}

func printedJob(t *testing.T, a *Agent, c Config) string {
	t.Helper()
	data, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	values, err := parsePlist(data)
	if err != nil {
		t.Fatal(err)
	}
	return a.target() + " = {\n\tpath = " + a.PlistPath + "\n\tprogram = " + c.Executable + "\n\targuments = {\n\t\t" + strings.Join(values["ProgramArguments"].Strings, "\n\t\t") + "\n\t}\n}\n"
}

func TestWakeGuardParsing(t *testing.T) {
	c := testConfig(t)
	a := installed(t, c)
	_, values, err := a.readOwned()
	if err != nil {
		t.Fatal(err)
	}
	job := printedJob(t, a, c)
	for _, bad := range []string{
		strings.Replace(job, "\n\tprogram = "+c.Executable, "\n\tprogram = /usr/bin/true", 1),
		strings.Replace(job, "\n\tpath = "+a.PlistPath, "\n\tpath = /private/tmp/other.plist", 1),
		strings.Replace(job, "\n\targuments = {", "\n\targuments = [", 1),
		job + "\n\tprogram = " + c.Executable + "\n",
	} {
		if loadedJobMatches(bad, a.target(), a.PlistPath, c.Executable, values["ProgramArguments"].Strings) {
			t.Fatal("accepted a mismatched or ambiguous loaded job")
		}
	}
	for _, tc := range []struct {
		body           string
		enabled, known bool
	}{
		{"disabled services = {\n}\n", true, true},
		{"disabled services = {\n\t\"" + a.Label + "\" => false\n}\n", true, true},
		{"disabled services = {\n\t\"" + a.Label + "\" => true\n}\n", false, true},
		{"unknown format", false, false},
		{"disabled services = {\n\t\"" + a.Label + "\" => maybe\n}\n", false, false},
	} {
		if enabled, known := jobEnabled(tc.body, a.Label); enabled != tc.enabled || known != tc.known {
			t.Fatalf("disabled output %q: %t %t", tc.body, enabled, known)
		}
	}
}

func TestLuaHook(t *testing.T) {
	plain, err := LuaHook("/opt/rexurrect", DefaultLabel, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "if rex.exec and rex.exec.start then\n  rex.exec.start({\"/opt/rexurrect\", \"agent\", \"wake\"}, {detach = true})\nend\n"
	if !strings.HasSuffix(string(plain), want) || strings.Contains(string(plain), "timeout") || !strings.HasPrefix(string(plain), "-- rexurrect:") {
		t.Fatalf("default snippet:\n%s", plain)
	}
	custom, err := LuaHook(`/dir with "quote"\and\backslash/rexurrect`, "io.test.label", "/plists")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(custom), `{"/dir with \"quote\"\\and\\backslash/rexurrect", "agent", "wake", "--label", "io.test.label", "--plist-dir", "/plists"}`) {
		t.Fatalf("custom snippet:\n%s", custom)
	}
	for _, bad := range [][3]string{{"relative/rexurrect", DefaultLabel, ""}, {"/opt/rexurrect", "bad label", ""}, {"/opt/rexurrect", DefaultLabel, "relative"}, {"/opt/rex\nurrect", DefaultLabel, ""}, {"/opt/rexurrect", DefaultLabel, "/plists\x7f"}} {
		if _, err := LuaHook(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	rex, err := exec.LookPath("rex")
	if err != nil {
		t.Skip("rex is not installed; the snippet syntax check needs it")
	}
	// The check really starts the detached command, so point it at true.
	snippet, err := LuaHook("/usr/bin/true", "io.blacktop.rexurrect.test.hook", filepath.Join(t.TempDir(), "LaunchAgents"))
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "init.lua")
	if err := os.WriteFile(config, snippet, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), rex, "--autostart=false", "config", "check", config, "--json").CombinedOutput()
	var report struct {
		Diagnostics []json.RawMessage `json:"diagnostics"`
	}
	if err != nil || json.Unmarshal(out, &report) != nil || len(report.Diagnostics) != 0 {
		t.Fatalf("rex config check rejected the snippet: %v\n%s", err, out)
	}
}
