import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

type Ctx = {
	mode?: string;
	agent?: { kind: string };
	sessionManager: { getSessionId(): string; isSessionOnDisk?(): boolean };
};

// rexurrect-launch grants ownership through RR_HOST, RR_HARNESS, and its own pid in
// RR_LAUNCHER_PID. Only the process the launcher started is the owner: a nested omp
// inherits the markers and the terminal but has another parent. Read once at load,
// because child sessions reuse this module.
const { RR_HOST, RR_HARNESS: command, RR_LAUNCHER_PID, REX_SESSION: session, REX_BLOCK: block } = process.env;
const { REXURRECT_SERVER: server, REXURRECT_STATE_DIR: stateDir } = process.env;
const owned =
	RR_HOST === "omp" && !!command && !!session && !!block && !!server && !!stateDir && process.ppid === Number(RR_LAUNCHER_PID);

export default function (pi: ExtensionAPI) {
	let registered: string | undefined;
	let inflight: string | undefined;
	let closing = false;
	let queued = false;
	let pending: Promise<void> = Promise.resolve();
	const abort = new AbortController();

	// Only the interactive main session owns the pane: print-mode children and subagents must not.
	const active = (ctx: Ctx) => owned && ctx.mode === "tui" && ctx.agent?.kind === "main";
	const receipt = (verb: "set" | "clear", id: string, wait: string, call: string, timeout: number, signal?: AbortSignal) =>
		pi.exec(
			"rexurrect",
			["receipt", verb, "--server", server!, "--state-dir", stateDir!, "--session", session!, "--block", block!, "--command", command!, "--conversation", id, "--lock-timeout", wait, "--call-timeout", call, ...(verb === "set" ? ["--host", "omp"] : [])],
			{ timeout, signal },
		);

	// A session that is not on disk yet has nothing to resume; a failed set is retried on the next trigger.
	const register = async (ctx: Ctx) => {
		const id = ctx.sessionManager.getSessionId();
		if (closing || !id || id === registered || !ctx.sessionManager.isSessionOnDisk?.()) return;
		inflight = id;
		try {
			const result = await receipt("set", id, "30s", "5s", 35_000, abort.signal);
			if (result.code === 0 && !result.killed) registered = id;
		} finally {
			inflight = undefined;
		}
	};
	// Runs in the background so startup never waits on the receipt lock; one set at a time, one more queued.
	const sync = (ctx: Ctx) => {
		if (!active(ctx) || closing || queued) return;
		queued = true;
		pending = pending.then(() => ((queued = false), register(ctx))).catch(() => {});
	};

	pi.on("session_start", (_event, ctx) => sync(ctx));
	pi.on("session_switch", (_event, ctx) => sync(ctx));
	pi.on("session_branch", (_event, ctx) => sync(ctx));
	pi.on("turn_end", (_event, ctx) => sync(ctx));
	pi.on("agent_end", (_event, ctx) => sync(ctx));
	pi.on("session_shutdown", async (_event, ctx) => {
		try {
			if (!active(ctx)) return;
			// Stop registering, abort a set that waits for the lock, then clear what it may have committed.
			// Shutdown only has a 2 s budget; the launcher clears again after omp exits if this misses.
			closing = true;
			const ids = new Set([registered, inflight]);
			abort.abort();
			await Promise.race([pending, new Promise(resolve => setTimeout(resolve, 500))]);
			await Promise.all([...ids].filter(id => id).map(id => receipt("clear", id!, "1s", "1s", 1300)));
		} catch {
			// Never let receipt bookkeeping disturb the session.
		}
	});
}
