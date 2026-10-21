#!/bin/sh
set -eu

# rexurrect-launch grants ownership explicitly: RR_HOST names the agent type
# and RR_HARNESS the command it was started as. Never infer an account from a
# home directory or process arguments.
case "${1-}:${RR_HOST:-}" in
codex:codex | claude:claude) rr_host=$1 ;;
*) exit 0 ;;
esac
rr_command=${RR_HARNESS:-}
[ -n "$rr_command" ] || exit 0
# Agent tool shells carry CODEX_THREAD_ID; Claude also sets CLAUDECODE in its
# own hooks, so only a Codex hook can treat that marker as nesting.
[ -z "${CODEX_THREAD_ID:-}" ] || exit 0
[ "$rr_host" = claude ] || [ -z "${CLAUDECODE:-}" ] || exit 0
[ -n "${REX_SESSION:-}" ] && [ -n "${REX_BLOCK:-}" ] || exit 0
rr_server=${REXURRECT_SERVER:?rexurrect-launch sets the Rex endpoint}
rr_state=${REXURRECT_STATE_DIR:?rexurrect-launch sets the state directory}

# Receipt models follow [a-z][a-z0-9]*([.-][a-z0-9]+)* and at most 128 bytes.
rr_valid_model() {
	[ "${#1}" -le 128 ] || return 1
	case "$1" in
	[!a-z]* | *[!a-z0-9.-]* | *[.-][.-]* | *[.-]) return 1 ;;
	esac
}

rr_receipt() {
	rexurrect receipt "$@" --server "$rr_server" --state-dir "$rr_state" \
		--session "$REX_SESSION" --block "$REX_BLOCK" --command "$rr_command" >/dev/null
}

rr_event=$(
	head -c 65537
	printf '.'
)
rr_event=${rr_event%.}
rr_size=$(printf '%s' "$rr_event" | wc -c | tr -d '[:space:]')
if [ "$rr_size" -gt 65536 ]; then
	printf 'rexurrect: hook input exceeds 64 KiB\n' >&2
	exit 1
fi
rr_kind=$(printf '%s' "$rr_event" | jq -er '.hook_event_name | select(type == "string")')
rr_id=$(printf '%s' "$rr_event" | jq -er '.session_id | select(type == "string")')
if [ "$rr_host" = claude ]; then
	rr_agent=$(printf '%s' "$rr_event" | jq -er '.agent_id // "" | select(type == "string")')
	[ -z "$rr_agent" ] || exit 0
fi

case "$rr_kind" in
SessionStart)
	# The manifests already filter sources per host; re-registering on
	# compaction is idempotent, so one list serves both hosts here.
	rr_source=$(printf '%s' "$rr_event" | jq -er '.source // "" | select(type == "string")')
	case "$rr_source" in
	startup | resume | clear | compact | fork) ;;
	*) exit 0 ;;
	esac
	if [ "$rr_host" = claude ] && [ -n "${CLAUDE_ENV_FILE:-}" ]; then
		# Later tool shells must not inherit this root session's ownership.
		rr_marker="export RR_HARNESS='' RR_HOST=''"
		if ! grep -qxF "$rr_marker" "$CLAUDE_ENV_FILE" 2>/dev/null; then
			printf '%s\n' "$rr_marker" >>"$CLAUDE_ENV_FILE"
		fi
	fi
	rr_model=$(printf '%s' "$rr_event" | jq -er '.model // "" | select(type == "string")')
	rr_model=${rr_model:-${RR_MODEL:-}}
	# Register without an unusable model so replay still resumes the conversation.
	if [ -n "$rr_model" ] && ! rr_valid_model "$rr_model"; then
		printf 'rexurrect: ignoring model %s; replay uses the account default\n' "$rr_model" >&2
		rr_model=
	fi
	rr_receipt set --call-timeout 1s --lock-timeout 30s --host "$rr_host" \
		--conversation "$rr_id" --model "$rr_model" --effort "${RR_EFFORT:-}"
	;;
SessionEnd)
	rr_receipt clear --call-timeout 1s --lock-timeout 1500ms --conversation "$rr_id"
	;;
esac
