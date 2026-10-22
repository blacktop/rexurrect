import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

type Ctx = {
	mode?: string;
	agent?: { kind: string };
	sessionManager: { getSessionId(): string; isSessionOnDisk?(): boolean };
};

// rexurrect-launch grants ownership through RR_HOST and RR_HARNESS. Read once at
// load: child sessions reuse this module. omp adds OMPCODE and CLAUDECODE to the
// shells it spawns, never to its own environment, and it snapshots that shell
// environment before extensions load, so clearing RR_* here would not reach tool
// shells. A nested omp therefore sees one of the agent markers and stays out.
const { RR_HOST, RR_HARNESS: command = "", REX_SESSION: session, REX_BLOCK: block, CLAUDECODE, CODEX_THREAD_ID, OMPCODE } = process.env;
const owned =
	RR_HOST === "omp" && /^[A-Za-z_][A-Za-z0-9_.-]{0,63}$/.test(command) && !!session && !!block && !CLAUDECODE && !CODEX_THREAD_ID && !OMPCODE;

// Same defaults and overrides as shell/rexurrect-launch.
const home = process.env.HOME ?? "";
const server = process.env.REXURRECT_SERVER || `unix://${home}/Library/Application Support/rex/server.sock`;
const stateDir = process.env.REXURRECT_STATE_DIR || `${home}/Library/Application Support/rexurrect`;

export default function (pi: ExtensionAPI) {
	let registered: string | undefined;
	let queued = false;
	let pending: Promise<void> = Promise.resolve();

	// Only the interactive main session owns the pane: print-mode children and subagents must not.
	const active = (ctx: Ctx) => owned && ctx.mode === "tui" && ctx.agent?.kind === "main";
	const receipt = (verb: "set" | "clear", id: string, wait: string, call: string, timeout: number) =>
		pi.exec(
			"rexurrect",
			["receipt", verb, "--server", server, "--state-dir", stateDir, "--session", session!, "--block", block!, "--command", command, "--conversation", id, "--lock-timeout", wait, "--call-timeout", call, ...(verb === "set" ? ["--host", "omp"] : [])],
			{ timeout },
		);

	// A session that is not on disk yet has nothing to resume; a failed set is retried on the next trigger.
	const register = async (ctx: Ctx) => {
		const id = ctx.sessionManager.getSessionId();
		if (!id || id === registered || !ctx.sessionManager.isSessionOnDisk?.()) return;
		const result = await receipt("set", id, "30s", "5s", 35_000);
		if (result.code === 0 && !result.killed) registered = id;
	};
	// Runs in the background so startup never waits on the receipt lock; one set at a time, one more queued.
	const sync = (ctx: Ctx) => {
		if (!active(ctx) || queued) return;
		queued = true;
		pending = pending.then(() => ((queued = false), register(ctx))).catch(() => {});
	};

	for (const event of ["session_start", "session_switch", "session_branch", "turn_end", "agent_end"] as const) {
		pi.on(event, (_event, ctx) => sync(ctx));
	}
	pi.on("session_shutdown", async (_event, ctx) => {
		try {
			if (!active(ctx)) return;
			await Promise.race([pending, new Promise(resolve => setTimeout(resolve, 300))]);
			// Shutdown only has a 2 s budget; the launcher clears again after omp exits if this misses.
			if (registered) await receipt("clear", registered, "1s", "1s", 1500);
		} catch {
			// Never let receipt bookkeeping disturb the session.
		}
	});
}
