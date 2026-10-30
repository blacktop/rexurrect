# Contributing

Keep changes focused, explain the problem they solve, and include the checks you
ran. For recovery bugs, include the Rex version and relevant status or journal
output, with anything private removed.

## Build

Development requires macOS, Rex, Go 1.27, and `just`. The tests also use Fish, Bun,
and `jq`.

```fish
brew install go just fish jq bun
git clone https://github.com/blacktop/rexurrect.git
cd rexurrect
just build
```

The binary is written to `bin/rexurrect`. To install it and enable the local
LaunchAgent, start Rex and run:

```fish
just install
just status
```

`just update` rebuilds and reinstalls after pulling changes. `just logs` follows
the automatic-cycle logs, and `just receipts` lists registered agent sessions.
The install recipes accept `REXURRECT_SERVER`, `REXURRECT_STATE_DIR`,
`REXURRECT_REX`, and `REXURRECT_BIN_DIR` overrides. See
[CLI resolution](docs/compatibility.md#cli-resolution) for their defaults.

## Checks

Run the full suite from a normal macOS terminal:

```fish
just check
```

It runs formatting, vet, and tests against private Rex servers, including a
The private servers start with `--restore=false`, so the installed `rex` must
accept that flag (server build 0.0.1793299046 or later).
temporary LaunchAgent with a unique label. The native checks need a real user
session and take several minutes; the launchd test waits through real timer
intervals.

Inside an agent sandbox, use the explicit skips:

```fish
env REXURRECT_SKIP_LAUNCHD=1 REXURRECT_SKIP_LOGIN_SHELL=1 just check
```

A passing run with those skips does not replace the native checks. Run the full
suite before merging. For plugin changes, also run the
[plugin validation commands](plugins/rexurrect/README.md#validate-without-installing).
The [compatibility document](docs/compatibility.md) records the tested behavior
and unsupported features.

## Releases

Maintainers publish from a clean `main` that has already been pushed. Install
GoReleaser, then run the release command with the next stable version:

```fish
brew install goreleaser
just release v0.1.1
```

The command runs the full native checks, validates GoReleaser's configuration,
creates a signed tag at the checked commit, and pushes only that tag. The
[GitHub workflow](.github/workflows/release.yml) publishes the release assets and
updates `Casks/rexurrect.rb` on `blacktop/homebrew-tap`. Use
`just release-check` to validate the configuration without publishing. If a tag
push fails, the command leaves the signed local tag in place and prints the
retry command.

The workflow needs `HOMEBREW_TAP_TOKEN` in the `release` environment of
`blacktop/rexurrect`. Use a fine-grained GitHub token limited to
`blacktop/homebrew-tap`, with repository Contents permission set to read and
write. Once the environment exists, add the token from your own terminal:

```fish
gh secret set HOMEBREW_TAP_TOKEN --repo blacktop/rexurrect --env release
gh secret list --repo blacktop/rexurrect --env release
```

Paste the token at the hidden prompt; keep it out of command arguments and
shell history. The current GoReleaser configuration skips Homebrew publication
for prerelease tags.
