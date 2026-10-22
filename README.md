<p align="center">
  <a href="https://github.com/blacktop/rexurrect"><img alt="Rexurrect Logo" src="docs/rexurrect_logo_readme.webp" width="200" height="200" /></a>
</p>
<h1 align="center">rexurrect</h1>

rexurrect restores local Rex sessions after a server restart or Mac reboot,
including tabs, splits, and working directories. The optional agent plugin also
resumes registered Claude Code and Codex conversations.

The project is experimental. Terminals without an agent receipt come back as
ordinary shells. With the optional shell hook, the command a pane was running
is typed back at its prompt (never run); with `--scrollback`, its earlier
output is replayed above the prompt.

## Install

With Rex running on macOS:

```sh
brew install --cask blacktop/tap/rexurrect
rexurrect agent install --server "unix://$HOME/Library/Application Support/rex/server.sock" --state-dir "$HOME/Library/Application Support/rexurrect"
rexurrect agent status
```

The LaunchAgent saves changed sessions every minute and responds when the Rex
socket appears or disappears. After a server restart, it restores the saved
sessions and starts saving again. Snapshots live in
`~/Library/Application Support/rexurrect`; the current and last-known-good
generations are retained.

For a source install, see [Building from source](CONTRIBUTING.md#build).
Custom paths and other options are covered in the
[CLI reference](docs/compatibility.md#cli-resolution).

## Resume agent conversations

Install `jq` with `brew install jq`, then add the plugin through the account
you use.

For Codex:

```sh
codex plugin marketplace add blacktop/rexurrect
codex plugin add rexurrect@rexurrect
```

For Claude Code:

```sh
claude plugin marketplace add blacktop/rexurrect
claude plugin install rexurrect@rexurrect --scope user
```

Finish the [wrapper and hook setup](plugins/rexurrect/README.md#give-the-root-agent-explicit-ownership),
then follow the [receipt checks](plugins/rexurrect/README.md#check-the-result).
The wrapper identifies which account owns the pane, so installing the plugin
alone isn't enough. Codex runs with `--no-daemon` inside Rex so its hooks follow
the pane's lifetime.

## Scrollback

`save --scrollback` and `auto --scrollback` also bring back each pane's earlier
output above its new prompt. It is off by default. The output is encrypted
before it reaches disk; the key lives in your login keychain, so a copied state
directory or a backup holds no readable terminal output, and a missing key
only costs the scrollback, never the layout or agent recovery. Full-screen
programs are skipped. Turn it on for the automatic job with
`rexurrect agent install --scrollback` (same `--server` and `--state-dir` as
before), or run `rexurrect save --scrollback` yourself; see
[Encrypted scrollback](docs/compatibility.md#encrypted-scrollback-s4).

## Running commands

Source `plugins/rexurrect/shell/foreground.zsh` from `~/.zshrc` or
`plugins/rexurrect/shell/foreground.fish` from `~/.config/fish/config.fish`.
While a command runs in a pane, the hook records the line you typed; when it
exits, the record goes away. After a restart, a pane that was running
something comes back with that line typed at its prompt, ready for Enter and
never run on its own. Agent panes still resume through their receipts. See
[Foreground commands](docs/compatibility.md#foreground-commands-s5).

## Usage

The automatic job handles routine recovery. Use `rexurrect status` to inspect
saved snapshots and the restore journal, supplying the same `--server` and
`--state-dir` used at installation.

Manual `restore` creates sessions. Running it against a server that still has
your sessions creates duplicates. See the
[save and status](docs/compatibility.md#save-and-status-s1a) and
[restore](docs/compatibility.md#journaled-restore-s1b) examples before using it.

- [Compatibility and limitations](docs/compatibility.md)
- [Agent plugin guide](plugins/rexurrect/README.md)
- [Development and releases](CONTRIBUTING.md)
- [Report an issue](https://github.com/blacktop/rexurrect/issues)
