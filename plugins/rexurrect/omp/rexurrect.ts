import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

type Ctx = {
	mode?: string;
	agent?: { kind: string };
	sessionManager: { getSessionId(): string; getSessionFile?(): string | undefined };
};

// Same defaults and overrides as shell/rexurrect-launch.
const home = process.env.HOME ?? "";
const server = process.env.REXURRECT_SERVER || `unix://${home}/Library/Application Support/rex/server.sock`;
const stateDir = process.env.REXURRECT_STATE_DIR || `${home}/Library/Application Support/rexurrect`;

export default function (pi: ExtensionAPI) {
	const receipt = async (verb: "set" | "clear", ctx: Ctx) => {
		try {
			const { REX_SESSION: session, REX_BLOCK: block } = process.env;
			// Only the interactive main session owns the pane: print-mode children, subagents and in-memory sessions must not.
			if (!session || !block || ctx.mode !== "tui" || ctx.agent?.kind !== "main" || !ctx.sessionManager.getSessionFile?.()) return;
			const id = ctx.sessionManager.getSessionId();
			if (!id) return;
			const args = ["receipt", verb, "--server", server, "--state-dir", stateDir, "--session", session, "--block", block, "--command", "omp", "--conversation", id, "--call-timeout", "1s", "--lock-timeout", "1500ms"];
			if (verb === "set") args.push("--host", "omp");
			// Shutdown only has a 2 s budget.
			await pi.exec("rexurrect", args, { timeout: 1800 });
		} catch {
			// Never let receipt bookkeeping disturb the session.
		}
	};

	for (const event of ["session_start", "session_switch", "session_branch"] as const) {
		pi.on(event, (_event, ctx) => receipt("set", ctx));
	}
	pi.on("session_shutdown", (_event, ctx) => receipt("clear", ctx));
}
