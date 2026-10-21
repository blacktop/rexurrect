---
name: setup
description: Configure the rexurrect plugin for explicit Codex or Claude Code conversation recovery in Rex, preserving the user's existing agent launch setup.
---

Help the user complete the integration described in [the package README](../../README.md).
The plugin is already a self-contained package; installing or enabling it does
not install rexurrect's binary or automatic job.

1. Run `rexurrect version`, `rexurrect receipt set --help`,
   `rexurrect receipt clear --help`, and `jq --version`. Confirm `set` accepts
   `--host` and `--command` and `clear` accepts the `--command` guard. For
   Codex, confirm `codex --help` lists `--no-daemon`. Use the endpoint and state directory the user supplies, or
   the README's macOS defaults. Do not inspect credentials, shell startup
   files, agent home directories, or environment dumps to infer an account.
2. Establish the user's shell and how they start each agent today: a plain
   executable on PATH, or a function that prepares an environment first.
   Preserve that setup and its argument vector. Do not invent launchers for
   agents the user does not run.
3. When the user authorizes shell integration, install
   `shell/rexurrect-launch` on PATH, then route `codex` and `claude` through
   it: `shell/rexurrect.sh` for bash or zsh, `shell/codex.fish` and
   `shell/claude.fish` for fish. If a function for that agent already exists,
   replace only its final launch with the matching `rexurrect-launch` line and
   pass the function's own name as the harness. A different executable goes in
   `REXURRECT_CODEX` or `REXURRECT_CLAUDE`, never into an edited copy of the
   launcher. Do not use `exec`; the launcher needs to perform cleanup after the
   process returns. Reload the shell before checking receipts.
4. Explain the hook review and trust flow. For Codex, start `codex --no-daemon`
   and review its hooks through `/hooks`; a daemon-attached TUI does not list
   new hooks. Do not restart a shared daemon or change persistent account
   settings as part of setup. Migrating a thread still held by that daemon
   needs separate user direction. Remove only an older rexurrect hook when the
   user chooses to replace it; preserve Rex's own hooks and unrelated entries.
   Never copy trust hashes or bypass hook trust.
5. Have the user launch a fresh conversation through the launcher in a Rex
   pane. Run the packaged `status` skill to verify the receipt's host, command, Rex
   block, and conversation UUID, then confirm a normal exit clears it. Check
   that a nested invocation cannot replace the enclosing receipt.

Report what was actually installed, configured, trusted, and exercised. Do not
claim live recovery from static checks or receipt presence. Restart Rex only
when the user expressly authorizes that acceptance check; never run manual
`rexurrect restore` against an active live server.
