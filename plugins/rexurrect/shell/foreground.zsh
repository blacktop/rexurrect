# rexurrect foreground recorder for zsh. Source this file from ~/.zshrc:
#
#   . /path/to/foreground.zsh
#
# While a command runs in a Rex pane, its command line is recorded under
# $REXURRECT_STATE_DIR/foreground; the record is removed when the command
# finishes. After a Rex restart, rexurrect types the recorded command at the
# restored prompt and never runs it. Only the outermost shell of a pane
# records, and a command typed with a leading space is not recorded.
if [[ -n ${REX_BLOCK-} && ${REX_BLOCK} == block:* && -z ${RR_FOREGROUND+set} ]]; then
	export RR_FOREGROUND=$REX_BLOCK
	typeset -g _rr_foreground_dir=${REXURRECT_STATE_DIR:-"$HOME/Library/Application Support/rexurrect"}/foreground
	typeset -g _rr_foreground_file=$_rr_foreground_dir/${REX_BLOCK#block:}
	_rr_foreground_preexec() {
		[[ $1 == ' '* ]] && return 0
		(
			umask 077
			tmp=$_rr_foreground_dir/.tmp-${REX_BLOCK#block:}.$$
			mkdir -p -- "$_rr_foreground_dir" &&
				printf '%s' "$1" >| "$tmp" &&
				mv -f -- "$tmp" "$_rr_foreground_file"
		) 2>/dev/null
		return 0
	}
	_rr_foreground_precmd() {
		rm -f -- "$_rr_foreground_file" 2>/dev/null
		return 0
	}
	autoload -Uz add-zsh-hook
	add-zsh-hook preexec _rr_foreground_preexec
	add-zsh-hook precmd _rr_foreground_precmd
fi
