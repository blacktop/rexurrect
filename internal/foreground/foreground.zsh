# rexurrect foreground recorder for zsh. Source this file from ~/.zshrc:
#
#   . /path/to/foreground.zsh
#
# While a command runs in a Rex pane, its command line is recorded under
# $REXURRECT_STATE_DIR/foreground; the record is removed when the command
# finishes. After a Rex restart, rexurrect types the recorded command at the
# restored prompt and never runs it. Only the outermost shell of a pane
# records, and a command typed with a leading space is not recorded.
#
# The hooks use zsh's own file builtins so that no command's start waits on a
# fork. Explicit modes make the umask irrelevant, and the record goes to a
# private temporary file that is renamed into place.
if [[ ${REX_BLOCK-} == block:* && -z ${RR_FOREGROUND+set} ]] &&
	zmodload -F zsh/files b:zf_mkdir b:zf_mv b:zf_rm 2>/dev/null &&
	zmodload zsh/system 2>/dev/null; then
	export RR_FOREGROUND=$REX_BLOCK
	typeset -g _rr_foreground_dir=${REXURRECT_STATE_DIR:-"$HOME/Library/Application Support/rexurrect"}/foreground
	typeset -g _rr_foreground_file=$_rr_foreground_dir/${REX_BLOCK#block:}
	_rr_foreground_preexec() {
		[[ $1 == ' '* ]] && return 0
		local fd tmp=$_rr_foreground_dir/.tmp-${REX_BLOCK#block:}.$$
		{
			zf_mkdir -p -m 700 -- "$_rr_foreground_dir" &&
				sysopen -w -o creat,trunc -m 600 -u fd -- "$tmp" || return 0
			# Only a fully written and closed record replaces the current one.
			if print -nr -u "$fd" -- "$1" && exec {fd}>&-; then
				zf_mv -f -- "$tmp" "$_rr_foreground_file"
			else
				exec {fd}>&-
				zf_rm -f -- "$tmp"
			fi
		} 2>/dev/null
		return 0
	}
	_rr_foreground_precmd() {
		zf_rm -f -- "$_rr_foreground_file" 2>/dev/null
		return 0
	}
	autoload -Uz add-zsh-hook
	add-zsh-hook preexec _rr_foreground_preexec
	add-zsh-hook precmd _rr_foreground_precmd
	# A normal exit runs preexec for the exit command but no precmd after it.
	add-zsh-hook zshexit _rr_foreground_precmd
fi
