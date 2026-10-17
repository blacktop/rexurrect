# rexurrect

Rex restarts, or the Mac reboots, and your sessions come back: the same tabs
and splits, each terminal in the directory it was in, and the Claude Code or
Codex conversation you were running resumed in its block.

rexurrect is a small Go tool for macOS. Saving only reads from Rex. Restoring
creates sessions, so that path keeps a write-ahead journal and refuses to
guess. It is experimental, and it is the only thing standing between you and an
empty Rex window after an update, so the defaults are conservative.

## What it does

`save` captures every session on an explicit Rex socket into a numbered
generation in a private state directory. Nothing on the server changes. A
capture identical to the previous one is not written again, and each
successful save keeps the current and last-known-good generations and prunes
the rest.

`restore` rebuilds the saved layouts as ordinary Rex terminals in their saved
working directories. Before each session it creates, it journals an intent with
a random marker, so a crash halfway through can be finished rather than
repeated. Two things worth knowing. A working directory that no longer exists
becomes your home directory. And restore never checks whether a session is
already there: run it against a live server that still has those sessions and
you get a second copy of each. To read the journal, use `status`.

`auto` is the one-shot cycle the LaunchAgent runs. It waits for the socket,
works out whether this is a new server instance, restores or finishes a pending
recovery if so, and then saves. On the same instance with nothing changed it
exits without writing.

`agent` installs, enables, disables, uninstalls, or reports on the per-user
LaunchAgent `io.blacktop.rexurrect`. launchd runs it whenever the Rex socket
appears or disappears, with a one-minute timer as a backstop. After a restart,
give it a few seconds before recreating anything by hand.

`receipt` opts one conversation into resume. `receipt set`, run from a
SessionStart hook, records the harness, conversation ID, model, and effort for
the current block. Restore types the matching `claude --resume` or
`codex resume` command into the rebuilt terminal. `receipt clear`, from a
SessionEnd hook, removes it. The harnesses are `claude`, `claude-ddb`, `codex`,
and `codex-team`. A `claude-team` receipt is recorded but never typed, and there
is no override for that.

Every Rex call gets a 20-second deadline, raised with `--call-timeout` up to 60
seconds. Only read-only calls are retried.

## Install

Homebrew ships the binary:

```fish
brew install --cask blacktop/tap/rexurrect
```

From a checkout with Go 1.27 and [just](https://just.systems/), `just install`
builds into `~/.local/bin` and installs the LaunchAgent for the local Rex
socket in one step. Rex must already be running. `just update` rebuilds after
pulling, `just status` shows the agent and the saved state, `just logs` tails
the cycle logs, and `just --list` has the rest.

The defaults are the local socket at
`~/Library/Application Support/rex/server.sock`, state in
`~/Library/Application Support/rexurrect`, and the `rex` CLI in `~/.local/bin`.
Override them with `REXURRECT_SERVER`, `REXURRECT_STATE_DIR`, `REXURRECT_REX`,
and `REXURRECT_BIN_DIR`. When an app update moves the Rex helper, the
[CLI resolver](docs/compatibility.md#cli-resolution) finds it and reports the
substitution.

With the Homebrew binary and no checkout, install the agent directly. The
interval, wait, and timeout default to one, two, and five minutes:

```fish
rexurrect agent install --server "unix://$HOME/Library/Application Support/rex/server.sock" --state-dir "$HOME/Library/Application Support/rexurrect"
```

## Resume hooks

rexurrect does not edit your dotfiles. The receipt is written by a hook you
install yourself: a SessionStart and SessionEnd hook for Claude Code, or a fish
wrapper around `codex resume`. Working examples of both are in the
[hook and wrapper examples](docs/compatibility.md#hook-and-wrapper-examples).

## Manual use

```sh
go build ./cmd/rexurrect
./rexurrect save --server unix:///absolute/path/rex.sock --state-dir ./saved
./rexurrect status --server unix:///absolute/path/rex.sock --state-dir ./saved --json
./rexurrect restore --server unix:///absolute/path/rex.sock --state-dir ./saved --json
./rexurrect auto --server unix:///absolute/path/rex.sock --state-dir ./saved --json
```

## Migration from rex-resurrect

This tool used to be called rex-resurrect, with the label
`com.blacktop.rex-resurrect`, a `rex-resurrect` state directory, and
`REX_RESURRECT_*` variables. Moving an existing installation is a one-time job,
and the order matters. The old agent, and any wrapper that started before you
edit your dotfiles, still holds the old state path. If one of them clears or
writes a receipt after the move, it recreates the old directory, and the new
agent either keeps a stale receipt or misses a new one. So: stop the old
writers, move the state, then update and reload the hooks.

The commands below use the old defaults. Substitute any binary path, label, or
state directory you customized. For a Homebrew install, use
`(command -s rex-resurrect)` as the old binary path.

1. Stop starting new agent sessions and other `rex-resurrect` commands.
2. Uninstall the old LaunchAgent with the old binary and label, so the
   automatic job stops writing. Continue only if this succeeds:

   ```fish
   "$HOME/.local/bin/rex-resurrect" agent uninstall --label com.blacktop.rex-resurrect
   ```

3. Let wrappers and hooks that started with the old binary or state path finish
   and clear their receipts, and let any in-flight `receipt`, `save`,
   `restore`, or `auto` command complete. Then confirm nothing old is still
   running. This must print nothing:

   ```fish
   pgrep -fl rex-resurrect
   ```

4. Move the state. The guard refuses if the new directory already exists;
   reconcile the two by hand in that case.

   ```fish
   test ! -e "$HOME/Library/Application Support/rexurrect"
   and mv "$HOME/Library/Application Support/rex-resurrect" "$HOME/Library/Application Support/rexurrect"
   ```

5. Update your hooks and wrappers to call `rexurrect` and rename every
   `REX_RESURRECT_*` override to `REXURRECT_*` (`SERVER`, `STATE_DIR`, `REX`,
   `BIN_DIR`), keeping the values except for the moved state path. Rename any
   `rex-resurrect-claude-hook` script and its hook settings to
   `rexurrect-claude-hook`, then reload them.
6. Install the new agent:

   ```fish
   just install
   ```

7. Start sessions again.

New journals use `rexurrect:` markers. Recovery still reads the old
`rex-resurrect:` markers in journals written before the rename. The longer
version is in the
[migration details](docs/compatibility.md#migration-from-rex-resurrect).

## Development

`make check` needs Go 1.27 and Rex installed. The integration tests start
private Rex servers of their own, never your live one, and cover
reconstruction, save and status, restore recovery, automatic cycles, agent
resume, and a PID-suffixed test LaunchAgent. The launchd and login-shell tests
need a real user session. Inside an agent sandbox, run
`env REXURRECT_SKIP_LAUNCHD=1 REXURRECT_SKIP_LOGIN_SHELL=1 make check`, and run
the full suite outside it before calling anything done.

Releases go through GoReleaser v2.18.2 in the GitHub workflow, which publishes
the Homebrew cask to `blacktop/homebrew-tap` with the `HOMEBREW_TAP_TOKEN`
secret. `just release-check` validates the config locally.
[compatibility.md](docs/compatibility.md) records the tested Rex version and
every boundary in detail.
