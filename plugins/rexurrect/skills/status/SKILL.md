---
name: status
description: Read-only checks for rexurrect's automatic job, current snapshot, and explicitly registered Codex or Claude conversations in Rex.
---

Use the user's stated Rex endpoint and rexurrect state directory. If none were
provided, use the macOS defaults documented in [the package README](../../README.md).
Do not infer account identity from home directories, process arguments, shell
startup files, credentials, or environment dumps.

Run these read-only commands with the selected endpoint and state directory:

- `rexurrect version`
- `rexurrect agent status --json`
- `rexurrect status --server ENDPOINT --state-dir DIRECTORY --json`
- `rexurrect receipt list --server ENDPOINT --state-dir DIRECTORY --json`

Use real argument values in place of `ENDPOINT` and `DIRECTORY`; preserve paths
with spaces as separate quoted arguments. Report whether the automatic job is
installed and loaded, which snapshot is current, and which relevant Rex blocks
have a registered host, command, and conversation. Highlight missing receipts,
inert legacy entries, failed cleanup, or mismatches with the account the user
expected. Do not expose unrelated private conversation identifiers in a broad
status summary.

Receipt registration proves only that recovery has an explicit command to
attempt. It does not prove successful login, model access, or an agent that
actually resumed. For a previously authorized restart check, inspect the
resulting restore status and then verify the actual conversation identities.
Do not run save, restore, auto, receipt set/clear, plugin installation, hook
trust changes, or a Rex restart as part of this read-only status skill.
