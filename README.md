<p align="center">
  <a href="https://github.com/blacktop/rexurrect"><img alt="Rexurrect Logo" src="docs/rexurrect_logo_readme.webp" width="200" height="200" /></a>
</p>
<h1 align="center">rexurrect</h1>
<p align="center">Restart Rex sessions after a reboot</p>

rexurrect saves your local Rex sessions and rebuilds them after a server restart
or Mac reboot. Your tabs, splits, and working directories come back. With the
agent plugin configured, registered Claude Code and Codex conversations can
resume in their original panes too.

The project is experimental. Saving only reads from Rex; recovery records each
step in a journal so an interrupted restore can continue. Terminals without an
agent receipt return as ordinary shells in their saved directories. Arbitrary
running commands and scrollback aren't restored.

## Install

You'll need macOS, a running Rex server, Go 1.27, and
[just](https://just.systems/). Build and install from the repository:

```fish
brew install go just
git clone https://github.com/blacktop/rexurrect.git
cd rexurrect
just install
just status
```

`just install` builds `~/.local/bin/rexurrect` and installs the per-user
LaunchAgent. After pulling updates, run `just update`. `just logs` follows the
automatic-cycle logs, and `just --list` shows the other maintenance commands.

The defaults are the local socket at
`~/Library/Application Support/rex/server.sock`, state in
`~/Library/Application Support/rexurrect`, and the `rex` CLI in `~/.local/bin`.
Override them with `REXURRECT_SERVER`, `REXURRECT_STATE_DIR`, `REXURRECT_REX`,
and `REXURRECT_BIN_DIR`. When an app update moves the Rex helper, the
[CLI resolver](docs/compatibility.md#cli-resolution) finds it and reports the
substitution.

There isn't a tagged binary release or Homebrew cask yet. The release workflow
is set up to publish `blacktop/tap/rexurrect` when the first release is tagged.

If you already have the binary, install the agent directly. The interval, wait,
and overall timeout default to one, two, and five minutes:

```fish
rexurrect agent install --server "unix://$HOME/Library/Application Support/rex/server.sock" --state-dir "$HOME/Library/Application Support/rexurrect"
```

## What it does

`save` captures every session on the chosen Rex socket into a numbered snapshot
in a private state directory. It keeps the current and last-known-good snapshots
and prunes older generations. Automatic saves skip unchanged captures.

`restore` rebuilds the saved layouts and resumes the conversations recorded in
them. Missing working directories fall back to your home directory. The journal
prevents an interrupted restore from recreating sessions it already created,
but it doesn't deduplicate unrelated sessions on the target server. Running a
manual restore against a server that still has your sessions creates copies.
Use `status` to inspect saved state and recovery progress.

`auto` runs one recovery-and-save cycle. It waits for Rex, checks the server
instance, finishes any required recovery, and saves the result. On the same
instance, it only needs to save changes.

`agent` manages the per-user LaunchAgent `io.blacktop.rexurrect`. launchd runs
the automatic cycle when the Rex socket appears or disappears, with a
one-minute timer as a backstop. After a restart, give it a few seconds before
recreating anything by hand.

`receipt` registers a conversation for resume. Lifecycle hooks record its
account command, conversation UUID, and any supported model and effort settings.
Recovery sends the corresponding `claude --resume` or `codex resume` command
to the rebuilt terminal. Normal exit clears the matching receipt. Supported
commands are `claude`, `claude-ddb`, `codex`, and `codex-team`. A user-started
`claude-team` session can have a receipt, but automatic recovery never starts it.

Every Rex call gets a 20-second deadline, raised with `--call-timeout` up to 60
seconds. Only read-only calls are retried.

## Resume hooks

The [portable agent plugin](plugins/rexurrect/README.md) bundles Codex and Claude
Code lifecycle hooks, setup/status skills, and Fish helpers for cleanup after an
agent exits. Install `jq`, then add the plugin through the account you use.

For Codex:

```fish
brew install jq
codex plugin marketplace add blacktop/rexurrect
codex plugin add rexurrect@rexurrect
```

For Claude Code:

```fish
brew install jq
claude plugin marketplace add blacktop/rexurrect
claude plugin install rexurrect@rexurrect --scope user
```

Then follow the [account-wrapper setup](plugins/rexurrect/README.md#give-the-root-agent-explicit-ownership)
and review the hooks through the account's normal trust flow. The wrapper tells
rexurrect which account command owns the pane. Installing the plugin alone
isn't enough, and rexurrect doesn't edit your dotfiles or account configuration
for you.

Open a fresh conversation in Rex and run `just receipts` from the checkout.
Check that its account command and UUID are correct, then exit normally and
check that the receipt disappears. The plugin guide also covers nested-agent
checks and verification after a user-initiated Rex restart.

The [manual hook and wrapper examples](docs/compatibility.md#hook-and-wrapper-examples)
remain available when you prefer to manage the integration yourself. Use one
rexurrect hook installation per account so the same event is not handled twice.

## Manual use

Use an explicit socket and state directory for each command. Run `restore` only
when you intend to create the saved sessions on that server.

```fish
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
state directory you customized. If the old binary is elsewhere on PATH, use
`(command -s rex-resurrect)` to find it.

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
the full suite from a normal macOS terminal before merging.

Releases go through GoReleaser v2.18.2 in the GitHub workflow, which publishes
the Homebrew cask to `blacktop/homebrew-tap` with the `HOMEBREW_TAP_TOKEN`
secret. `just release-check` validates the config locally.
[compatibility.md](docs/compatibility.md) records the tested Rex version and
every boundary in detail.
