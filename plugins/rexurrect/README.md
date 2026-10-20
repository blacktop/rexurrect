# rexurrect agent plugin

This package connects Codex and Claude Code lifecycle hooks to rexurrect's
explicit receipt store. A fresh conversation records its own UUID and account
wrapper; normal exit clears only the matching receipt. The package includes
Fish helpers for reliable cleanup after the agent process returns.

The plugin needs an installed `rexurrect` with `receipt clear --harness` and
`receipt set|clear --lock-timeout`, `rex`, and `jq` on the hook's PATH. Install
and configure rexurrect's automatic job separately. The plugin does not install
the binary, edit account configuration, or start or restart Rex.

## Install the plugin

The repository's `.claude-plugin/marketplace.json` is a shared catalog that
Codex and Claude Code can load. Add it from GitHub in your own terminal:

```fish
codex plugin marketplace add blacktop/rexurrect
codex plugin add rexurrect@rexurrect
```

For Claude Code, choose the installation scope you want:

```fish
claude plugin marketplace add blacktop/rexurrect
claude plugin install rexurrect@rexurrect --scope user
```

For local development, replace `blacktop/rexurrect` in the marketplace command
with the absolute path to your checkout.

Use each account's existing command to install and enable the plugin in that
account's configuration. Keep its current authentication and routing intact.
In Codex, review the two plugin hook definitions with `/hooks` and trust them
using that account's normal trust flow. Do not copy trust hashes or bypass hook
trust. In Claude Code, review the enabled plugin and its hooks with `/plugin`
and `/hooks`. Remove an older manually configured rexurrect receipt hook for
that account when switching to the plugin, so the same event is not registered
twice. Preserve unrelated hooks, including Rex's own integration.

The portable manifest selects `hooks/codex.json` for Codex, and the Claude
manifest selects `hooks/claude.json`. There is deliberately no default
`hooks/hooks.json` to register the wrong host's handlers. OpenAI currently
supports lifecycle hooks for manually installed plugins but excludes them from
its public plugin directory; distribute this package through a local or repository
marketplace until that policy changes.

## Give the root agent explicit ownership

An enabled plugin needs an exact account marker to know which command should
be resumed. Hooks accept `RR_HARNESS=codex`, `codex-team`, `claude`, or
`claude-ddb` for the corresponding host. A user-initiated `claude-team` session
can record a receipt when its existing wrapper explicitly supplies that marker,
but rexurrect never resumes it automatically and this package supplies no
launcher for that account.

The default server and state directory are:

```text
unix://$HOME/Library/Application Support/rex/server.sock
$HOME/Library/Application Support/rexurrect
```

If your automatic job uses other locations, export `REXURRECT_SERVER` and
`REXURRECT_STATE_DIR` in the account wrapper before its launch. These values
must agree with the job. Do not set `RR_HARNESS` globally: it belongs to one
foreground agent invocation.

From your own Fish terminal, copy the two helpers from a stable checkout into
Fish's autoload directory. `cp -n` keeps any existing implementation for you to
review first:

```fish
set -l rr_plugin /absolute/path/to/rexurrect/plugins/rexurrect
mkdir -p "$HOME/.config/fish/functions"
cp -n "$rr_plugin/shell/_rexurrect_codex.fish" "$HOME/.config/fish/functions/"
cp -n "$rr_plugin/shell/_rexurrect_claude.fish" "$HOME/.config/fish/functions/"
```

If either destination already exists, compare it with the packaged helper and
merge or replace it after preserving any custom executable selection. Do this
before continuing: the older resume-only helper does not supply the ownership
marker required by this plugin. Reload the updated function or open a new Fish
terminal before checking receipts.

If `codex` and `claude` currently resolve only to executables, install the
provided entry points too:

```fish
cp -n "$rr_plugin/shell/codex.fish" "$HOME/.config/fish/functions/"
cp -n "$rr_plugin/shell/claude.fish" "$HOME/.config/fish/functions/"
```

The supplied helpers launch `codex` or `claude` from PATH. If an existing account
function already uses that executable, retain its account setup and replace only
the final launch. Use the matching line for that function:

```fish
_rexurrect_codex codex $argv
_rexurrect_codex codex-team $argv
_rexurrect_claude claude $argv
_rexurrect_claude claude-ddb $argv
```

If a wrapper selects an absolute executable path or another launcher, adapt a
copy of the corresponding helper to keep that exact target in both launch
branches. Do not replace it with the PATH executable merely to use this package.

Do not use `exec`: the helper must regain control for its cleanup attempt.
Remove the older resume-only registration and cleanup from wrappers using the
helper. Existing matching `RR_HARNESS` values are accepted; conflicting owners,
agent tool shells, and nested helper calls launch with that marker empty and
skip receipt cleanup. Arguments, account environment, and process exit status
are preserved. A cleanup failure is reported instead of returning a successful
exit status. Concurrent foreground wrappers in the same Rex block are not
supported.

Codex registers startup, resume, clear, and fork events; compaction preserves
the existing receipt. Claude also refreshes its receipt on compaction. Claude
subagents and in-process teammates do not own the parent terminal's receipt.
The Claude startup hook clears the ownership marker in future tool shells via
`CLAUDE_ENV_FILE`. It records the model reported by Claude, falling back to the
wrapper's `RR_MODEL`, and records the wrapper's `RR_EFFORT`. Explicit Claude
`--model` and `--effort` arguments override those wrapper defaults. Codex's
payload supplies its model but no effort, so replay uses the account's effort
default.

## Check the result

Run the packaged `setup` skill for a guided installation or the `status` skill
for a read-only report. In a Rex pane, open a fresh conversation through the
configured account wrapper. In another pane, list receipts with the same server
and state directory as the automatic job:

```fish
set -l rr_server "unix://$HOME/Library/Application Support/rex/server.sock"
set -l rr_state "$HOME/Library/Application Support/rexurrect"
rexurrect receipt list --server "$rr_server" --state-dir "$rr_state" --json
```

Check the Rex block, exact harness, and conversation UUID. After a normal exit,
the matching receipt must disappear. Check that nested agent commands preserve
the enclosing receipt. A final recovery check requires a user-authorized Rex
restart and verification of the actual resumed conversations. A complete layout
journal alone does not establish successful agent login or resumption. Never
run `rexurrect restore` against an active live server as a plugin setup test.

The short SessionEnd hook is a best-effort first cleanup attempt. The Fish
helper retries cleanup after the process exits, with rexurrect's normal lock
wait. Launches outside the helper lack that backstop.

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
- [Claude plugin manifest](https://code.claude.com/docs/en/plugins/manifest-reference)
- [Claude lifecycle hooks](https://code.claude.com/docs/en/hooks)
- [Portable Agent Plugins specification](https://agent-plugins.org/specification)
