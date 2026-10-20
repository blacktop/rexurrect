set shell := ["bash", "-euo", "pipefail", "-c"]
set positional-arguments

export RR_BIN_DIR := env("REXURRECT_BIN_DIR", home_directory() / ".local/bin")
export RR_SERVER := env("REXURRECT_SERVER", "unix://" + home_directory() / "Library/Application Support/rex/server.sock")
export RR_STATE_DIR := env("REXURRECT_STATE_DIR", home_directory() / "Library/Application Support/rexurrect")
export RR_REX := env("REXURRECT_REX", home_directory() / ".local/bin/rex")

default:
    just --list

build:
    mkdir -p ./bin
    go build -o ./bin/rexurrect ./cmd/rexurrect

# Run formatting, vet, and the full private-server test suite.
check:
    test -z "$(gofmt -l cmd internal)"
    go vet ./...
    go test -p 1 ./...
    go test ./internal/... -run TestIsolatedRoundTrip -v
    go test ./internal/... -run TestSaveStatusPrivateServer -v
    go test ./internal/... -run TestRestoreRecoveryPrivateServer -v
    go test ./internal/... -run TestAutoCyclePrivateServer -v
    go test ./cmd/... -run TestAutoBinaryPrivateServer -v
    go test ./internal/... -run TestLaunchAgentLifecycle -v
    go test ./internal/rex -run TestDefaultTerminalRoundTrip -v
    go test ./internal/rex -run TestAgentResumeRoundTrip -v

# Resolve the configured Rex CLI during installation; preserve it in the plist.
install *args:
    mkdir -p "$RR_BIN_DIR"
    go build -o "$RR_BIN_DIR/rexurrect" ./cmd/rexurrect
    "$RR_BIN_DIR/rexurrect" agent install --server "$RR_SERVER" --state-dir "$RR_STATE_DIR" --rex "$RR_REX" "$@"

update *args:
    just install "$@"

status *args:
    "$RR_BIN_DIR/rexurrect" agent status
    "$RR_BIN_DIR/rexurrect" status --server "$RR_SERVER" --state-dir "$RR_STATE_DIR" --rex "$RR_REX" --json "$@"

logs:
    tail -n 50 -F "$RR_STATE_DIR/auto.stdout.log" "$RR_STATE_DIR/auto.stderr.log"

enable *args:
    "$RR_BIN_DIR/rexurrect" agent enable "$@"

disable *args:
    "$RR_BIN_DIR/rexurrect" agent disable "$@"

uninstall *args:
    "$RR_BIN_DIR/rexurrect" agent uninstall "$@"

receipts *args:
    "$RR_BIN_DIR/rexurrect" receipt list --server "$RR_SERVER" --state-dir "$RR_STATE_DIR" --rex "$RR_REX" --json "$@"

release-check:
    goreleaser check

# Check, sign, and push a stable release tag, for example: just release v0.1.0.
release version:
    #!/usr/bin/env bash
    set -euo pipefail
    rr_version="$1"
    if [[ ! "$rr_version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
        printf 'Invalid release tag %q; expected vMAJOR.MINOR.PATCH\n' "$rr_version" >&2
        exit 1
    fi
    rr_head=$(git rev-parse --verify 'HEAD^{commit}')
    rr_ref="refs/tags/$rr_version"
    rr_require_main() {
        local rr_branch rr_current rr_status rr_remote
        rr_branch=$(git branch --show-current)
        rr_current=$(git rev-parse --verify 'HEAD^{commit}')
        rr_status=$(git status --porcelain)
        if [[ "$rr_branch" != main || "$rr_current" != "$rr_head" || -n "$rr_status" ]]; then
            printf 'Release requires a clean main checkout at %s\n' "$rr_head" >&2
            exit 1
        fi
        rr_remote=$(git ls-remote --exit-code origin refs/heads/main)
        if [[ "$rr_remote" != "$rr_head"$'\trefs/heads/main' ]]; then
            printf 'Push main before releasing; origin/main must match %s\n' "$rr_head" >&2
            exit 1
        fi
    }
    rr_require_main
    if git show-ref --verify --quiet "$rr_ref"; then
        printf 'Tag %s already exists locally; inspect it before retrying a push\n' "$rr_version" >&2
        exit 1
    fi
    rr_remote_tag=$(git ls-remote --tags origin "$rr_ref")
    if [[ -n "$rr_remote_tag" ]]; then
        printf 'Tag %s already exists on origin\n' "$rr_version" >&2
        exit 1
    fi
    env REXURRECT_SKIP_LAUNCHD=0 REXURRECT_SKIP_LOGIN_SHELL=0 just check
    just release-check
    rr_require_main
    git tag -s "$rr_version" "$rr_head" -m "Release $rr_version"
    if ! git push --no-follow-tags origin "$rr_ref"; then
        printf 'Tag %s remains local. After resolving the push failure, retry: git push --no-follow-tags origin %s\n' "$rr_version" "$rr_ref" >&2
        exit 1
    fi
    printf 'Pushed %s; GitHub Actions will publish the release and Homebrew cask.\n' "$rr_version"
