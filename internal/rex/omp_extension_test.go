package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// ompHarness runs the real omp extension with a stub `pi` that records every
// exec call, fires the scripted events with ctx objects, and prints what it
// saw. Ownership is read when the extension module loads, so every scenario
// needs its own bun process. A deferred exec waits for its abort signal, the
// way a receipt set waits for the state lock.
const ompHarness = `
const scenario = JSON.parse(process.env.OMP_SCENARIO!);
const handlers: Record<string, ((event: unknown, ctx: unknown) => unknown)[]> = {};
const calls: { args: string[]; timeout?: number; aborted: boolean }[] = [];
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));
let next = 0;
const pi = {
	on(event: string, handler: (event: unknown, ctx: unknown) => unknown) {
		(handlers[event] ??= []).push(handler);
	},
	async exec(cmd: string, args: string[], opts?: { timeout?: number; signal?: AbortSignal }) {
		if (cmd !== "rexurrect") throw new Error("unexpected command " + cmd);
		const call = { args, timeout: opts?.timeout, aborted: false };
		calls.push(call);
		const result = scenario.exec?.[next++] ?? {};
		if (result.defer) {
			return new Promise((resolve) =>
				opts?.signal?.addEventListener("abort", () => {
					call.aborted = true;
					resolve({ stdout: "", stderr: "", code: 0, killed: true });
				}),
			);
		}
		return { stdout: "", stderr: "", code: result.code ?? 0, killed: result.killed ?? false };
	},
};
const extension = await import(process.env.OMP_EXT!);
extension.default(pi);

const cur: Record<string, any> = { mode: "tui", agent: "main", id: "", disk: true };
for (const step of scenario.steps) {
	Object.assign(cur, step.ctx ?? {});
	const ctx = {
		mode: cur.mode,
		agent: cur.agent ? { kind: cur.agent } : undefined,
		sessionManager: { getSessionId: () => cur.id, isSessionOnDisk: () => cur.disk },
	};
	for (const handler of handlers[step.event] ?? []) await handler({}, ctx);
	await sleep(25);
}
console.log(JSON.stringify({ calls }));
process.exit(0);
`

type ompStep struct {
	Event string         `json:"event"`
	Ctx   map[string]any `json:"ctx,omitzero"`
}

type ompExec struct {
	Code   int  `json:"code,omitzero"`
	Killed bool `json:"killed,omitzero"`
	Defer  bool `json:"defer,omitzero"`
}

// ompCall is one `rexurrect receipt` invocation the extension asked pi.exec for.
type ompCall struct {
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
	Aborted bool     `json:"aborted"`
}

// ompWant is the call the extension must make: set waits for the receipt lock
// as long as the hooks do; clear stays inside omp's 2 s shutdown budget and
// carries the conversation guard.
type ompWant struct {
	verb, id string
	aborted  bool
}

func (w ompWant) call() ompCall {
	r := agentReceipt(1, "")
	args := []string{"receipt", w.verb, "--server", ompServer, "--state-dir", ompState,
		"--session", r.SessionID, "--block", r.BlockID, "--command", ompCommand, "--conversation", w.id}
	if w.verb == "set" {
		return ompCall{Args: append(args, "--lock-timeout", "30s", "--call-timeout", "5s", "--host", "omp"),
			Timeout: 35000, Aborted: w.aborted}
	}
	return ompCall{Args: append(args, "--lock-timeout", "1s", "--call-timeout", "1s"), Timeout: 1300, Aborted: w.aborted}
}

const (
	ompServer  = "unix:///private/tmp/rex-plugin-test-unused"
	ompState   = "/private/tmp/rex-plugin-test-state"
	ompCommand = "omp-work"
	ompIDA     = "01a15c18-8aae-7774-8c25-1eb59ff7f355"
	ompIDB     = "01a161db-6d74-77be-bdb9-212f9646e748"
)

func ompEvent(event string, ctx map[string]any) ompStep { return ompStep{Event: event, Ctx: ctx} }

func ompStart(id string, disk bool) ompStep {
	return ompEvent("session_start", map[string]any{"id": id, "disk": disk})
}

func TestPluginOmpExtension(t *testing.T) {
	t.Parallel()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	extension, err := filepath.Abs("../../plugins/rexurrect/omp/rexurrect.ts")
	if err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(t.TempDir(), "harness.ts")
	if err := os.WriteFile(harness, []byte(ompHarness), 0600); err != nil {
		t.Fatal(err)
	}
	var (
		shutdown  = ompEvent("session_shutdown", nil)
		turnEnd   = ompEvent("turn_end", nil)
		agentEnd  = ompEvent("agent_end", nil)
		lifecycle = []ompStep{ompStart(ompIDA, true), turnEnd, shutdown}
		setA      = ompWant{verb: "set", id: ompIDA}
		setB      = ompWant{verb: "set", id: ompIDB}
		clearA    = ompWant{verb: "clear", id: ompIDA}
		clearB    = ompWant{verb: "clear", id: ompIDB}
	)
	cases := []struct {
		name  string
		steps []ompStep
		exec  []ompExec
		env   []string
		want  []ompWant
	}{
		{name: "owner registers once and clears its own", want: []ompWant{setA, clearA},
			steps: []ompStep{ompStart(ompIDA, true), turnEnd, agentEnd, ompEvent("session_branch", nil), shutdown}},
		{name: "fresh session registers once it is durable", want: []ompWant{setA, clearA},
			steps: []ompStep{ompStart(ompIDA, false), turnEnd, ompEvent("agent_end", map[string]any{"disk": true}), turnEnd, shutdown}},
		{name: "session that never reaches disk registers and clears nothing", want: []ompWant{},
			steps: []ompStep{ompStart(ompIDA, false), turnEnd, shutdown}},
		{name: "failed and killed sets retry on the next trigger", want: []ompWant{setA, setA, setA, clearA},
			exec:  []ompExec{{Code: 1}, {Killed: true}, {}},
			steps: []ompStep{ompStart(ompIDA, true), turnEnd, agentEnd, turnEnd, shutdown}},
		{name: "failed set leaves nothing to clear", want: []ompWant{setA},
			exec: []ompExec{{Code: 1}}, steps: []ompStep{ompStart(ompIDA, true), shutdown}},
		{name: "changed id registers again and clear uses the registered id", want: []ompWant{setA, setB, clearB},
			steps: []ompStep{ompStart(ompIDA, true), ompEvent("session_switch", map[string]any{"id": ompIDB}), turnEnd,
				ompEvent("session_shutdown", map[string]any{"id": ompIDA})}},
		{name: "shutdown aborts a set waiting for the lock and clears its id", want: []ompWant{{verb: "set", id: ompIDA, aborted: true}, clearA},
			exec: []ompExec{{Defer: true}}, steps: []ompStep{ompStart(ompIDA, true), turnEnd, shutdown}},
		{name: "launcher pid of another process owns nothing", want: []ompWant{}, env: []string{"RR_LAUNCHER_PID=1"}, steps: lifecycle},
		{name: "missing launcher pid owns nothing", want: []ompWant{}, env: []string{"RR_LAUNCHER_PID="}, steps: lifecycle},
		{name: "missing host owns nothing", want: []ompWant{}, env: []string{"RR_HOST="}, steps: lifecycle},
		{name: "subagent owns nothing", want: []ompWant{}, steps: []ompStep{
			ompEvent("session_start", map[string]any{"id": ompIDA, "agent": "sub"}), turnEnd, shutdown}},
		{name: "missing agent owns nothing", want: []ompWant{}, steps: []ompStep{
			ompEvent("session_start", map[string]any{"id": ompIDA, "agent": ""}), turnEnd, shutdown}},
		{name: "print mode owns nothing", want: []ompWant{}, steps: []ompStep{
			ompEvent("session_start", map[string]any{"id": ompIDA, "mode": "print"}), turnEnd, shutdown}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scenario, err := json.Marshal(map[string]any{"steps": tc.steps, "exec": tc.exec})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, bun, harness)
			cmd.WaitDelay = time.Second
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			// The harness is the extension's parent process only for the owner.
			cmd.Env = append(paneEnv(t.TempDir(), filepath.Dir(bun), ompServer, ompState),
				"RR_HOST=omp", "RR_HARNESS="+ompCommand, "RR_LAUNCHER_PID="+strconv.Itoa(os.Getpid()),
				"OMP_EXT="+extension, "OMP_SCENARIO="+string(scenario))
			cmd.Env = append(cmd.Env, tc.env...)
			if err := cmd.Run(); err != nil {
				t.Fatalf("harness(%q): %v (context: %v)\nstderr: %s", tc.name, err, ctx.Err(), stderr.Bytes())
			}
			var got struct {
				Calls []ompCall `json:"calls"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("harness(%q) output %q: %v\nstderr: %s", tc.name, stdout.Bytes(), err, stderr.Bytes())
			}
			want := make([]ompCall, 0, len(tc.want))
			for _, w := range tc.want {
				want = append(want, w.call())
			}
			if !reflect.DeepEqual(got.Calls, want) {
				t.Errorf("harness(%q) calls = %+v, want %+v\nstderr: %s", tc.name, got.Calls, want, stderr.Bytes())
			}
		})
	}
}
