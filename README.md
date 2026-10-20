<p align="center">
  <a href="https://github.com/blacktop/rexurrect"><img alt="Rexurrect Logo" src="docs/rexurrect_logo_readme.webp" width="200" height="200" /></a>
</p>
<h1 align="center">rexurrect</h1>
<p align="center">Restart Rex sessions after a reboot</p>

rexurrect restores local Rex sessions after a server restart or Mac reboot,
including tabs, splits, and working directories. The optional agent plugin also
resumes registered Claude Code and Codex conversations.

The project is experimental. Terminals without an agent receipt come back as
ordinary shells. Arbitrary running commands and scrollback aren't restored.

## Install

With Rex running on macOS:

```fish
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

```fish
codex plugin marketplace add blacktop/rexurrect
codex plugin add rexurrect@rexurrect
```

For Claude Code:

```fish
claude plugin marketplace add blacktop/rexurrect
claude plugin install rexurrect@rexurrect --scope user
```

Finish the [wrapper and hook setup](plugins/rexurrect/README.md#give-the-root-agent-explicit-ownership),
then follow the [receipt checks](plugins/rexurrect/README.md#check-the-result).
The wrapper identifies which account owns the pane, so installing the plugin
alone isn't enough. `claude-team` always stays manual.

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
