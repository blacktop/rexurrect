package rex

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ompHarness runs the real omp extension with a stub `pi` that records every
// exec call, fires the scripted events with ctx objects, and prints what it
// saw. Ownership is read when the extension module loads, so every scenario
// needs its own bun process.
const ompHarness = `
const scenario = JSON.parse(process.env.OMP_SCENARIO!);
const handlers: Record<string, ((event: unknown, ctx: unknown) => unknown)[]> = {};
const calls: { cmd: string; args: string[]; timeout?: number }[] = [];
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));
let next = 0;
const pi = {
	on(event: string, handler: (event: unknown, ctx: unknown) => unknown) {
		(handlers[event] ??= []).push(handler);
	},
	async exec(cmd: string, args: string[], opts?: { timeout?: number }) {
		calls.push({ cmd, args, timeout: opts?.timeout });
		const result = scenario.exec?.[next++] ?? {};
		if (result.hang) return new Promise(() => {});
		return { stdout: "", stderr: "", code: result.code ?? 0, killed: result.killed ?? false };
	},
};
const extension = await import(process.env.OMP_EXT!);
extension.default(pi);

const cur: Record<string, any> = { mode: "tui", agent: "main", id: "", disk: true };
const counts: number[] = [];
const ms: number[] = [];
for (const step of scenario.steps) {
	Object.assign(cur, step.ctx ?? {});
	const ctx = {
		mode: cur.mode,
		agent: cur.agent ? { kind: cur.agent } : undefined,
		sessionManager: {
			getSessionId: () => cur.id,
			isSessionOnDisk: cur.noMethod ? undefined : () => cur.disk,
		},
	};
	const started = performance.now();
	for (const handler of handlers[step.event] ?? []) await Promise.race([handler({}, ctx), sleep(3000)]);
	ms.push(performance.now() - started);
	await sleep(25);
	counts.push(calls.length);
}
console.log(JSON.stringify({ events: Object.keys(handlers).sort(), calls, counts, ms }));
process.exit(0);
`

type ompStep struct {
	Event string         `json:"event"`
	Ctx   map[string]any `json:"ctx,omitempty"`
}

type ompExec struct {
	Code   int  `json:"code,omitempty"`
	Killed bool `json:"killed,omitempty"`
	Hang   bool `json:"hang,omitempty"`
}

type ompCall struct {
	Cmd     string   `json:"cmd"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

type ompResult struct {
	Events []string  `json:"events"`
	Calls  []ompCall `json:"calls"`
	Counts []int     `json:"counts"`
	MS     []float64 `json:"ms"`
}

type ompCase struct {
	name  string
	steps []ompStep
	exec  []ompExec
	env   []string
	// want lists the expected receipt calls as "set ID" or "clear ID".
	want []string
	// counts, when set, is the cumulative number of exec calls after each step.
	counts []int
	// command, server, and state default to the launcher's markers and the pane
	// environment.
	command, server, state string
	// maxMS limits the time an event handler may block, per step.
	maxMS float64
}

const (
	ompSessionA = "01a15c18-8aae-7774-8c25-1eb59ff7f355"
	ompSessionB = "01a161db-6d74-77be-bdb9-212f9646e748"
)

func ompStart(id string, disk bool) ompStep {
	return ompStep{Event: "session_start", Ctx: map[string]any{"id": id, "disk": disk}}
}

func ompEvent(event string, ctx map[string]any) ompStep { return ompStep{Event: event, Ctx: ctx} }

// wantOmpCall is the exact exec call the extension must make: set waits for
// the receipt lock as long as the hooks do, clear stays inside omp's 2 s
// shutdown budget and carries the conversation guard.
func wantOmpCall(verb, id, command, server, state string) ompCall {
	args := []string{"receipt", verb, "--server", server, "--state-dir", state,
		"--session", agentReceipt(1, "").SessionID, "--block", agentReceipt(1, "").BlockID,
		"--command", command, "--conversation", id}
	if verb == "set" {
		return ompCall{Cmd: "rexurrect", Timeout: 35000,
			Args: append(args, "--lock-timeout", "30s", "--call-timeout", "5s", "--host", "omp")}
	}
	return ompCall{Cmd: "rexurrect", Timeout: 1500,
		Args: append(args, "--lock-timeout", "1s", "--call-timeout", "1s")}
}

func TestOmpExtension(t *testing.T) {
	t.Parallel()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	extension, err := filepath.Abs("../../plugins/rexurrect/omp/rexurrect.ts")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	harness := filepath.Join(dir, "harness.ts")
	if err := os.WriteFile(harness, []byte(ompHarness), 0600); err != nil {
		t.Fatal(err)
	}
	const server, state = "unix:///private/tmp/rex-plugin-test-unused", "/private/tmp/rex-plugin-test-state"
	lifecycle := []ompStep{
		ompStart(ompSessionA, true),
		ompEvent("turn_end", nil),
		ompEvent("session_shutdown", nil),
	}
	nested := func(name string, env ...string) ompCase {
		return ompCase{name: name, env: env, steps: lifecycle}
	}
	cases := []ompCase{
		{name: "owner-registers-once-and-clears-its-own",
			steps: []ompStep{ompStart(ompSessionA, true), ompEvent("turn_end", nil), ompEvent("agent_end", nil),
				ompEvent("session_branch", nil), ompEvent("session_switch", nil), ompEvent("session_shutdown", nil)},
			want:   []string{"set " + ompSessionA, "clear " + ompSessionA},
			counts: []int{1, 1, 1, 1, 1, 2}},
		{name: "command-is-the-launcher-harness", command: "omp-work", env: []string{"RR_HARNESS=omp-work"},
			steps: lifecycle, want: []string{"set " + ompSessionA, "clear " + ompSessionA}},
		{name: "default-endpoint-comes-from-home", server: "unix://<home>/Library/Application Support/rex/server.sock",
			state: "<home>/Library/Application Support/rexurrect", env: []string{"REXURRECT_SERVER=", "REXURRECT_STATE_DIR="},
			steps: lifecycle, want: []string{"set " + ompSessionA, "clear " + ompSessionA}},
		{name: "fresh-session-registers-once-durable",
			steps: []ompStep{ompStart(ompSessionA, false), ompEvent("session_switch", nil),
				ompEvent("turn_end", map[string]any{"disk": true}), ompEvent("agent_end", nil),
				ompEvent("session_shutdown", nil)},
			want:   []string{"set " + ompSessionA, "clear " + ompSessionA},
			counts: []int{0, 0, 1, 1, 2}},
		{name: "session-never-durable-registers-and-clears-nothing",
			steps: []ompStep{ompStart(ompSessionA, false), ompEvent("turn_end", nil), ompEvent("session_shutdown", nil)}},
		{name: "host-without-durability-signal-registers-nothing",
			steps: []ompStep{ompEvent("session_start", map[string]any{"id": ompSessionA, "noMethod": true}),
				ompEvent("turn_end", nil), ompEvent("session_shutdown", nil)}},
		{name: "failed-set-retries-on-next-trigger",
			exec:  []ompExec{{Code: 1}, {Killed: true}, {Code: 1, Killed: true}, {}},
			steps: []ompStep{ompStart(ompSessionA, true), ompEvent("turn_end", nil), ompEvent("agent_end", nil), ompEvent("turn_end", nil), ompEvent("agent_end", nil), ompEvent("session_shutdown", nil)},
			want: []string{"set " + ompSessionA, "set " + ompSessionA, "set " + ompSessionA, "set " + ompSessionA,
				"clear " + ompSessionA},
			counts: []int{1, 2, 3, 4, 4, 5}},
		{name: "failed-set-registers-nothing-to-clear",
			exec:  []ompExec{{Code: 1}},
			steps: []ompStep{ompStart(ompSessionA, true), ompEvent("session_shutdown", nil)},
			want:  []string{"set " + ompSessionA}},
		{name: "changed-id-registers-again-and-clear-uses-registered-id",
			steps: []ompStep{ompStart(ompSessionA, true),
				ompEvent("session_switch", map[string]any{"id": ompSessionB}),
				ompEvent("turn_end", map[string]any{"id": ompSessionB}),
				ompEvent("agent_end", map[string]any{"id": ompSessionA}),
				ompEvent("session_shutdown", map[string]any{"id": ompSessionB})},
			want: []string{"set " + ompSessionA, "set " + ompSessionB, "set " + ompSessionA, "clear " + ompSessionA}},
		{name: "shutdown-guards-the-registered-conversation",
			steps: []ompStep{ompStart(ompSessionA, true), ompEvent("session_shutdown", map[string]any{"id": ompSessionB})},
			want:  []string{"set " + ompSessionA, "clear " + ompSessionA}},
		{name: "hung-lock-never-blocks-startup-or-shutdown",
			exec: []ompExec{{Hang: true}}, maxMS: 1000,
			steps: []ompStep{ompStart(ompSessionA, true), ompEvent("turn_end", nil), ompEvent("session_shutdown", nil)},
			want:  []string{"set " + ompSessionA}},
		{name: "subagent-owns-nothing", steps: []ompStep{
			ompEvent("session_start", map[string]any{"id": ompSessionA, "agent": "sub"}),
			ompEvent("turn_end", nil), ompEvent("session_shutdown", nil)}},
		{name: "missing-agent-owns-nothing", steps: []ompStep{
			ompEvent("session_start", map[string]any{"id": ompSessionA, "agent": ""}),
			ompEvent("turn_end", nil), ompEvent("session_shutdown", nil)}},
		nested("nested-without-markers", "RR_HOST=", "RR_HARNESS="),
		nested("nested-without-host", "RR_HOST="),
		nested("nested-without-harness", "RR_HARNESS="),
		nested("nested-foreign-host", "RR_HOST=claude"),
		nested("nested-invalid-harness", "RR_HARNESS=not a command"),
		nested("nested-missing-session", "REX_SESSION="),
		nested("nested-missing-block", "REX_BLOCK="),
		nested("nested-in-claude", "CLAUDECODE=1"),
		nested("nested-in-codex", "CODEX_THREAD_ID=outer"),
		nested("nested-in-omp-tool-shell", "OMPCODE=1", "CLAUDECODE=1"),
		nested("nested-in-omp-marker-only", "OMPCODE=1"),
	}
	for _, mode := range []string{"print", "json", "rpc", ""} {
		cases = append(cases, ompCase{name: "mode-" + cmp.Or(mode, "unset") + "-owns-nothing", steps: []ompStep{
			ompEvent("session_start", map[string]any{"id": ompSessionA, "mode": mode}),
			ompEvent("turn_end", nil), ompEvent("session_shutdown", nil)}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			tc.command = cmp.Or(tc.command, "omp")
			tc.server = cmp.Or(tc.server, server)
			tc.state = cmp.Or(tc.state, state)
			tc.server = strings.ReplaceAll(tc.server, "<home>", home)
			tc.state = strings.ReplaceAll(tc.state, "<home>", home)
			scenario, err := json.Marshal(map[string]any{"steps": tc.steps, "exec": tc.exec})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bun, harness)
			cmd.Env = append(paneEnv(home, filepath.Dir(bun), server, state),
				"RR_HOST=omp", "RR_HARNESS=omp", "OMP_EXT="+extension, "OMP_SCENARIO="+string(scenario))
			cmd.Env = append(cmd.Env, tc.env...)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("harness failed: %v\n%s", err, out)
			}
			var got ompResult
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("harness output %q: %v", out, err)
			}
			wantEvents := []string{"agent_end", "session_branch", "session_shutdown", "session_start", "session_switch", "turn_end"}
			if !slices.Equal(got.Events, wantEvents) {
				t.Fatalf("extension registered %v, want %v", got.Events, wantEvents)
			}
			var want []ompCall
			for _, w := range tc.want {
				verb, id, _ := strings.Cut(w, " ")
				want = append(want, wantOmpCall(verb, id, tc.command, tc.server, tc.state))
			}
			if len(got.Calls) != len(want) {
				t.Fatalf("exec calls:\n%+v\nwant:\n%+v", got.Calls, want)
			}
			for i := range want {
				if got.Calls[i].Cmd != want[i].Cmd || got.Calls[i].Timeout != want[i].Timeout ||
					!slices.Equal(got.Calls[i].Args, want[i].Args) {
					t.Fatalf("call %d:\n%+v\nwant:\n%+v", i, got.Calls[i], want[i])
				}
			}
			if tc.counts != nil && !slices.Equal(got.Counts, tc.counts) {
				t.Fatalf("cumulative calls %v, want %v", got.Counts, tc.counts)
			}
			if tc.maxMS > 0 {
				for i, took := range got.MS {
					if took > tc.maxMS {
						t.Fatalf("step %d blocked omp for %.0f ms, limit %.0f", i, took, tc.maxMS)
					}
				}
			}
		})
	}
}
