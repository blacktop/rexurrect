# Shell functions for bash and zsh. Source this file from ~/.bashrc or
# ~/.zshrc after PATH includes rexurrect-launch:
#
#   . /path/to/rexurrect.sh
#
# Each function's name is the harness rexurrect replays after a restart.
codex() { rexurrect-launch codex codex "$@"; }
claude() { rexurrect-launch claude claude "$@"; }
