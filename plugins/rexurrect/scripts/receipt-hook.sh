#!/bin/sh
set -eu

# A wrapper grants ownership explicitly. Never infer an account from its home.
rr_host=${1-}
rr_harness=${RR_HARNESS:-}
case "$rr_host:$rr_harness" in
codex:codex | codex:codex-team)
	[ -z "${CLAUDECODE:-}" ] && [ -z "${CODEX_THREAD_ID:-}" ] || exit 0
	;;
claude:claude | claude:claude-ddb | claude:claude-team)
	# Claude sets CLAUDECODE in its own hooks, including the root session.
	[ -z "${CODEX_THREAD_ID:-}" ] || exit 0
	;;
*) exit 0 ;;
esac
[ -n "${REX_SESSION:-}" ] && [ -n "${REX_BLOCK:-}" ] || exit 0

# Receipt models follow [a-z][a-z0-9]*([.-][a-z0-9]+)* and at most 128 bytes.
rr_valid_model() {
	[ "${#1}" -le 128 ] || return 1
	case "$1" in
	[!a-z]* | *[!a-z0-9.-]* | *[.-][.-]* | *[.-]) return 1 ;;
	esac
}

rr_server=${REXURRECT_SERVER:-"unix://$HOME/Library/Application Support/rex/server.sock"}
rr_state=${REXURRECT_STATE_DIR:-"$HOME/Library/Application Support/rexurrect"}
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
	rr_source=$(printf '%s' "$rr_event" | jq -er '.source // "" | select(type == "string")')
	case "$rr_host:$rr_source" in
	codex:startup | codex:resume | codex:clear | codex:fork) ;;
	claude:startup | claude:resume | claude:clear | claude:compact | claude:fork) ;;
	*) exit 0 ;;
	esac
	rr_model=$(printf '%s' "$rr_event" | jq -er '.model // "" | select(type == "string")')
	rr_effort=
	if [ "$rr_host" = claude ]; then
		# Later tool shells must not inherit this root session's ownership.
		if [ -n "${CLAUDE_ENV_FILE:-}" ] && ! grep -qxF "export RR_HARNESS=''" "$CLAUDE_ENV_FILE" 2>/dev/null; then
			printf "export RR_HARNESS=''\n" >>"$CLAUDE_ENV_FILE"
		fi
		rr_model=${rr_model:-${RR_MODEL:-}}
		rr_effort=${RR_EFFORT:-}
	fi
	# Register without an unusable model so replay still resumes the conversation.
	if [ -n "$rr_model" ] && ! rr_valid_model "$rr_model"; then
		printf 'rexurrect: ignoring model %s; replay uses the account default\n' "$rr_model" >&2
		rr_model=
	fi
	rexurrect receipt set --server "$rr_server" --state-dir "$rr_state" \
		--call-timeout 1s --lock-timeout 30s \
		--session "$REX_SESSION" --block "$REX_BLOCK" --harness "$rr_harness" \
		--conversation "$rr_id" --model "$rr_model" --effort "$rr_effort" >/dev/null
	;;
SessionEnd)
	rexurrect receipt clear --server "$rr_server" --state-dir "$rr_state" \
		--call-timeout 1s --lock-timeout 1500ms \
		--session "$REX_SESSION" --block "$REX_BLOCK" --harness "$rr_harness" \
		--conversation "$rr_id" >/dev/null
	;;
esac
