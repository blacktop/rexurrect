---
name: setup
description: Configure the rexurrect plugin for explicit Codex or Claude Code conversation recovery in Rex, preserving existing account routing.
---

Help the user complete the integration described in [the package README](../../README.md).
The plugin is already a self-contained package; installing or enabling it does
not install rexurrect's binary or automatic job.

1. Run `rexurrect version`, `rexurrect receipt set --help`,
   `rexurrect receipt clear --help`, and `jq --version`. Confirm the CLI exposes
   the harness guard and lock timeout. Use the endpoint and state directory the
   user supplies, or the README's macOS defaults. Do not inspect credentials,
   shell startup files, account homes, or environment dumps to infer an account.
2. Establish the host and the exact existing account command: `codex`,
   `codex-team`, `claude`, or `claude-ddb`. Preserve its account environment and
   original argument vector. Never introduce an account fallback. `claude-team`
   is manual-only; do not create a launcher or automatic resume path for it.
3. Explain the per-account hook review and normal trust flow. Remove only an
   older rexurrect hook when the user chooses to replace it; preserve Rex's own
   hooks and unrelated entries. Never copy trust hashes or bypass hook trust.
4. When the user authorizes shell integration, use the packaged
   `shell/_rexurrect_codex.fish` and `shell/_rexurrect_claude.fish` helpers.
   If a helper already exists, merge or replace it after preserving any custom
   executable target; a skipped `cp -n` is not a completed upgrade. Reload the
   updated function before checking receipts.
   The supplied helpers invoke the normal PATH executables. Existing account
   functions using those targets keep their setup and replace only their final
   launch. If a function selects a different executable or launcher, adapt both
   helper launch branches to preserve that exact target. For a plain executable
   with no existing Fish function, the complete
   entry points are `shell/codex.fish` and `shell/claude.fish`. Do not overwrite
   an existing account function without adapting its current route. Do not use
   `exec`; the helper needs to perform cleanup after the process returns.
5. Have the user launch a fresh conversation through the selected wrapper in a
   Rex pane. Run the packaged `status` skill to verify the receipt's exact
   harness, Rex block, and conversation UUID, then confirm normal exit clears
   it. Check that a nested invocation cannot replace the enclosing receipt.

Report what was actually installed, configured, trusted, and exercised. Do not
claim live recovery from static checks or receipt presence. Restart Rex only
when the user expressly authorizes that acceptance check; never run manual
`rexurrect restore` against an active live server.
