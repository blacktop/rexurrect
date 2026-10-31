
# rexurrect delivery hook for fish. After a Rex restart that brought this
# pane back as a bare shell, rexurrect asks the shell itself to put the
# pane's saved command back: it publishes a request under
# $REXURRECT_STATE_DIR/deliver/<block>/ and sends this shell SIGURG, a
# signal fish ignores unless a handler is installed, so a shell without
# this hook is never affected. The handler below runs between keystrokes,
# claims the request by renaming it, checks that the request is addressed
# to this hook and that the command line is empty, and only then puts the
# command there, running it for an agent resume (mode run) and leaving it
# typed for a recorded command (mode type). Every claimed request is
# acknowledged with its attempt id, so nothing is ever delivered twice.
# Only an interactive shell that is the outermost shell of a pane installs;
# a shell that replaced it with exec, which keeps the pid, installs again
# and announces itself afresh.
if status is-interactive; and set -q REX_BLOCK; and string match -q 'block:*' -- $REX_BLOCK; and begin
        not set -q RR_DELIVERY; or test "$RR_DELIVERY" = "$REX_BLOCK:$fish_pid"
    end
    set -gx RR_DELIVERY "$REX_BLOCK:$fish_pid"
    set -l state_dir "$HOME/Library/Application Support/rexurrect"
    set -q REXURRECT_STATE_DIR; and set state_dir $REXURRECT_STATE_DIR
    set -g _rr_deliver_dir "$state_dir/deliver/"(string replace 'block:' '' -- $REX_BLOCK)
    set -g _rr_deliver_id "$fish_pid."(random)(random)(random)
    function _rr_deliver_write --argument-names name content
        set -l tmp "$_rr_deliver_dir/.tmp-$name.$fish_pid"
        set -l old_umask (umask)
        umask 077
        begin
            printf '%s\n' "$content" >"$tmp"
            and command mv -f -- "$tmp" "$_rr_deliver_dir/$name"
        end 2>/dev/null
        umask $old_umask
        return 0
    end
    function _rr_deliver --on-signal SIGURG
        set -l claimed "$_rr_deliver_dir/claimed.$_rr_deliver_id"
        command mv -f -- "$_rr_deliver_dir/request" "$claimed" 2>/dev/null
        or return 0
        set -l header (command head -n 1 -- "$claimed" | string split ' ')
        set -l cmd (command tail -n +2 -- "$claimed" | string collect)
        command rm -f -- "$claimed"
        if test (count $header) -ne 3
            return 0
        end
        if test "$header[2]" != "$_rr_deliver_id"
            _rr_deliver_write ack "$header[1] rejected:other_hook"
            return 0
        end
        set -l buffer (commandline --current-buffer)
        if test -n "$buffer"
            _rr_deliver_write ack "$header[1] skipped:pending_input"
            return 0
        end
        commandline -r -- "$cmd"
        if test "$header[3]" = run
            commandline -f execute
        else
            commandline -f repaint
        end
        _rr_deliver_write ack "$header[1] delivered:$header[3]"
        return 0
    end
    function _rr_deliver_exit --on-event fish_exit
        command rm -f -- "$_rr_deliver_dir/ready" "$_rr_deliver_dir/request" "$_rr_deliver_dir/ack" "$_rr_deliver_dir"/claimed.* 2>/dev/null
        command rmdir -- "$_rr_deliver_dir" 2>/dev/null
        return 0
    end
    begin
        set -l old_umask (umask)
        umask 077
        command mkdir -p -- "$_rr_deliver_dir" 2>/dev/null
        umask $old_umask
    end
    _rr_deliver_write ready "$fish_pid fish $_rr_deliver_id"
end
