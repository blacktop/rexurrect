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

check:
    make check

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
