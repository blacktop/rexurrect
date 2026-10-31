
# rexurrect delivery hook for zsh. After a Rex restart that brought this
# pane back as a bare shell, rexurrect asks the shell itself to put the
# pane's saved command back: it publishes a request under
# $REXURRECT_STATE_DIR/deliver/<block>/ and writes a line into the fifo this
# shell keeps open. The widget below runs inside the line editor, claims
# the request by renaming it, checks that the request is addressed to this
# hook and that the command line is empty, and only then puts the command
# there. It never runs it: zsh cannot accept a line from a handler before
# the next keystroke, so the command waits for Enter. Every claimed request
# is acknowledged with its attempt id, so nothing is ever delivered twice.
# Only an interactive shell that is the outermost shell of a pane installs;
# a shell that replaced it with exec, which keeps the pid, installs again
# and announces itself afresh.
if [[ -o interactive && ${REX_BLOCK-} == block:* && ( -z ${RR_DELIVERY+set} || $RR_DELIVERY == "$REX_BLOCK:$$" ) ]] &&
	zmodload -F zsh/files b:zf_mkdir b:zf_mv b:zf_rm b:zf_rmdir 2>/dev/null &&
	zmodload zsh/system 2>/dev/null; then
	export RR_DELIVERY="$REX_BLOCK:$$"
	typeset -g _rr_deliver_dir=${REXURRECT_STATE_DIR:-"$HOME/Library/Application Support/rexurrect"}/deliver/${REX_BLOCK#block:}
	typeset -g _rr_deliver_id="$$.$RANDOM$RANDOM$RANDOM"
	typeset -gi _rr_deliver_fd=-1
	_rr_deliver_write() {
		local fd tmp=$_rr_deliver_dir/.tmp-$1.$$
		{
			sysopen -w -o creat,trunc -m 600 -u fd -- "$tmp" || return 0
			if print -r -u "$fd" -- "$2" && exec {fd}>&-; then
				zf_mv -f -- "$tmp" "$_rr_deliver_dir/$1"
			else
				exec {fd}>&-
				zf_rm -f -- "$tmp"
			fi
		} 2>/dev/null
		return 0
	}
	_rr_deliver_widget() {
		local line claimed=$_rr_deliver_dir/claimed.$_rr_deliver_id
		local -a lines header
		read -r -u $_rr_deliver_fd line 2>/dev/null
		zf_mv -f -- "$_rr_deliver_dir/request" "$claimed" 2>/dev/null || return 0
		lines=("${(@f)$(<"$claimed")}")
		zf_rm -f -- "$claimed"
		header=(${=lines[1]})
		if (( $#header != 3 )); then
			return 0
		fi
		if [[ $header[2] != $_rr_deliver_id ]]; then
			_rr_deliver_write ack "$header[1] rejected:other_hook"
			return 0
		fi
		if [[ -n $BUFFER ]]; then
			_rr_deliver_write ack "$header[1] skipped:pending_input"
			return 0
		fi
		BUFFER=${(F)lines[2,-1]}
		CURSOR=${#BUFFER}
		zle -R
		_rr_deliver_write ack "$header[1] delivered:type"
		return 0
	}
	zle -N _rr_deliver_widget
	_rr_deliver_exit() {
		zf_rm -f -- "$_rr_deliver_dir/ready" "$_rr_deliver_dir/request" "$_rr_deliver_dir/ack" "$_rr_deliver_dir/fifo" "$_rr_deliver_dir"/claimed.*(N) 2>/dev/null
		zf_rmdir -- "$_rr_deliver_dir" 2>/dev/null
		return 0
	}
	autoload -Uz add-zsh-hook
	add-zsh-hook zshexit _rr_deliver_exit
	# The fifo is opened for reading and writing so it never reads end of
	# file, and close-on-exec so no command this shell starts inherits it: an
	# inherited copy would keep the fifo open after the hook is gone, and a
	# wake written into it would then wait out the full acknowledgement
	# timeout instead of failing at once.
	if zf_mkdir -p -m 700 -- "$_rr_deliver_dir" 2>/dev/null &&
		{ [[ -p $_rr_deliver_dir/fifo ]] || command mkfifo -m 600 -- "$_rr_deliver_dir/fifo"; } 2>/dev/null &&
		sysopen -r -w -o cloexec -u _rr_deliver_fd -- "$_rr_deliver_dir/fifo" 2>/dev/null; then
		zle -F -w $_rr_deliver_fd _rr_deliver_widget
		_rr_deliver_write ready "$$ zsh $_rr_deliver_id"
	fi
fi
