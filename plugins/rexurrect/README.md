# rexurrect agent plugin

This package connects Codex and Claude Code lifecycle hooks to rexurrect's
explicit receipt store. A fresh conversation records its own UUID and the
command that launched it; a normal exit clears only the matching receipt.
After a Rex restart, rexurrect types that command's `resume` form into the
restored pane.

The plugin needs an installed `rexurrect` whose `receipt set` accepts `--host`
and `--command` (receipt store format 2), plus `rex` and `jq` on the hook's
PATH.
Interactive Codex needs `--no-daemon`, present since Codex CLI 0.163.0. Install
and configure rexurrect's automatic job separately. The plugin does not install
the binary, edit account configuration, or start or restart Rex.

## Install the plugin

The repository's `.claude-plugin/marketplace.json` is a shared catalog that
Codex and Claude Code can load. Add it from GitHub in your own terminal:

```sh
codex plugin marketplace add blacktop/rexurrect
codex plugin add rexurrect@rexurrect
```

For Claude Code, choose the installation scope you want:

```sh
claude plugin marketplace add blacktop/rexurrect
claude plugin install rexurrect@rexurrect --scope user
```

For local development, replace `blacktop/rexurrect` in the marketplace command
with the absolute path to your checkout.

Hooks run only after you review them. In Codex, start `codex --no-daemon`
and trust the two plugin hook definitions through `/hooks`; a TUI attached to
the shared daemon does not list newly installed hooks. In Claude Code, review
the enabled plugin and its hooks with `/plugin` and `/hooks`. Do not copy trust
hashes or bypass hook trust. If you previously configured a manual rexurrect
hook, remove only that entry so the same event is not registered twice, and
keep Rex's own integration hooks.

The `.codex-plugin/plugin.json` manifest selects `hooks/codex.json`, and the
Claude manifest selects `hooks/claude.json`. There is deliberately no default
`hooks/hooks.json`, which would register the wrong host's handlers. OpenAI
currently supports lifecycle hooks for manually installed plugins but excludes
them from its public plugin directory; distribute this package through a local
or repository marketplace until that policy changes.

## Give the root agent explicit ownership

A hook cannot tell which pane owns the conversation on its own, so the agent
has to be started through a small launcher. `shell/rexurrect-launch` is a
POSIX `sh` script that works from any shell. Install it on your PATH:

```sh
install -m 0755 /path/to/rexurrect/plugins/rexurrect/shell/rexurrect-launch "$HOME/.local/bin/"
```

Then make `codex` and `claude` go through it. The function name is the
harness: it is what rexurrect types after a restart, so it must resolve in
your login shell.

bash or zsh (`~/.bashrc` or `~/.zshrc`):

```sh
. /path/to/rexurrect/plugins/rexurrect/shell/rexurrect.sh
```

fish (autoloaded functions):

```sh
cp /path/to/rexurrect/plugins/rexurrect/shell/codex.fish "$HOME/.config/fish/functions/"
cp /path/to/rexurrect/plugins/rexurrect/shell/claude.fish "$HOME/.config/fish/functions/"
```

Both files are one line per command: `rexurrect-launch codex codex "$@"` and
`rexurrect-launch claude claude "$@"`. If you already wrap an agent in a
function that sets up its environment, keep that setup and replace only the
final launch with the matching `rexurrect-launch` line; pass the function's own
name as the second argument. Do not `exec` it: the launcher must regain control
to clear the receipt. Any bare command name works (letters, digits, `_`, `.`,
`-`, at most 64 bytes); the first argument, the agent type, decides the resume
syntax rexurrect types.

What the launcher does:

- Exports `RR_HOST` (the agent type) and `RR_HARNESS` (the command name) to
  the agent so the hooks can prove which pane owns the conversation. Any
  marker already present, even empty, means a launcher or agent session is
  above this one; such launches, agent tool shells, and launches outside a Rex
  pane run the agent directly with both markers blank.
- Inside Rex, starts Codex with `--no-daemon`. The shared Codex app-server
  daemon outlives a pane, runs hooks with its own environment, and does not end
  a thread when the pane closes, so a daemon-attached session can never be
  registered to its pane. An explicit `--no-daemon` is not duplicated, and
  `codex agents`, `codex app-server`, and `codex remote-control` run unchanged
  (with or without global options before them) because they exist to talk to
  that daemon.
- Records `--model`/`-m` and `--effort` or `-c model_reasoning_effort=` from
  the arguments as defaults for the hook, because the hook payloads omit the
  effort and Claude's may omit the model.
- After the agent exits, clears the pane's receipt registered under this
  command name with rexurrect's normal lock wait. The SessionEnd hook is a
  best-effort first attempt under its host's short deadline; this is the
  backstop.
- Preserves the arguments, the environment, and the exit status. A failed
  cleanup returns its own error instead of the agent's status.

To launch an executable that is not the PATH `codex` or `claude`, set
`REXURRECT_CODEX` or `REXURRECT_CLAUDE` in the function before calling the
launcher. If rexurrect's automatic job uses a non-default endpoint or state
directory, export `REXURRECT_SERVER` and `REXURRECT_STATE_DIR` there too. The
defaults are:

```text
unix://$HOME/Library/Application Support/rex/server.sock
$HOME/Library/Application Support/rexurrect
```

Do not set `RR_HOST` or `RR_HARNESS` globally: they belong to one foreground
agent invocation. Concurrent foreground launches in the same Rex block are not
supported.

Foreground Codex sessions are not live tasks in the shared `codex agents`
view. Closing the session ends its local process; its saved conversation can
still be resumed by UUID. Codex refuses to reopen a thread that a running
daemon still holds and asks you to close it in the other app first; quitting
only the attached TUI can leave the thread open.

Codex registers startup, resume, clear, and fork events; a resumed interactive
thread may emit SessionStart only on its first turn, by which time restore has
already rebound its receipt. Compaction preserves the existing receipt. Claude
also refreshes its receipt on compaction. Claude subagents do not own the
parent terminal's receipt, and the Claude startup hook clears the ownership
markers in that session's tool shells through `CLAUDE_ENV_FILE`. Codex's
payload supplies its model but no effort, so replay uses the account's effort
default.

## omp

omp needs no launcher. `omp/rexurrect.ts` is an omp extension; symlink or copy
it into omp's extension directory:

```sh
ln -s /path/to/rexurrect/plugins/rexurrect/omp/rexurrect.ts "$HOME/.omp/agent/extensions/rexurrect.ts"
```

In a Rex pane, the interactive main session registers its receipt on start,
switch, and branch, and clears it on exit; subagents and `omp -p` do not. After
a restart, rexurrect types `omp --resume <session-id>`. The installed
`rexurrect` must accept `--host omp`, and `omp` must resolve in your login
shell.

## Check the result

Run the packaged `setup` skill for a guided installation or the `status` skill
for a read-only report. In a Rex pane, open a fresh conversation through the
launcher. In another pane, list receipts with the same server and state
directory as the automatic job:

```sh
rexurrect receipt list --server "unix://$HOME/Library/Application Support/rex/server.sock" --state-dir "$HOME/Library/Application Support/rexurrect" --json
```

Check the Rex block, host, command, and conversation UUID. After a normal exit, the
matching receipt must disappear. Check that a nested agent command preserves
the enclosing receipt. A final recovery check requires a Rex restart that you
start yourself, followed by verification of the actual resumed conversations.
A complete layout journal alone does not establish successful agent login or
resumption. Never run `rexurrect restore` against an active live server as a
plugin setup test.

## Validate without installing

From the rexurrect checkout, run the package tests and Claude's strict validators:

```sh
go test ./internal/rex -run '^TestPlugin' -count=1
claude plugin validate --strict plugins/rexurrect
claude plugin validate --strict .claude-plugin/marketplace.json
```

Codex can inspect the local catalog with temporary configuration overrides.
Replace the path below with the checkout's absolute path. This lists the package
without adding a marketplace or installing/enabling a plugin:

```sh
codex -c 'marketplaces.rexurrect.source_type="local"' -c 'marketplaces.rexurrect.source="/absolute/path/to/rexurrect"' plugin list --marketplace rexurrect --available --json
```

## References

- [OpenAI plugin package format](https://developers.openai.com/plugins/build/plugins)
- [Codex lifecycle hooks](https://learn.chatgpt.com/docs/hooks)
- [Codex CLI reference](https://learn.chatgpt.com/docs/cli/reference)
- [Claude plugin manifest](https://code.claude.com/docs/en/plugins/manifest-reference)
- [Claude lifecycle hooks](https://code.claude.com/docs/en/hooks)
