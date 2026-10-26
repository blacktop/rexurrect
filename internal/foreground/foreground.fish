# rexurrect foreground recorder for fish. Source this file from
# ~/.config/fish/config.fish:
#
#   source /path/to/foreground.fish
#
# While a command runs in a Rex pane, its command line is recorded under
# $REXURRECT_STATE_DIR/foreground; the record is removed when the command
# finishes. After a Rex restart, rexurrect types the recorded command at the
# restored prompt and never runs it. Only the outermost shell of a pane
# records, and a command typed with a leading space is not recorded.
#
# fish has no file builtins, so the record's rename and removal each cost one
# external command, called through `command` so user functions named rm, mv,
# or mkdir never take part; the directory is created once here, not per
# command.
if set -q REX_BLOCK; and string match -q 'block:*' -- $REX_BLOCK; and not set -q RR_FOREGROUND
    set -gx RR_FOREGROUND $REX_BLOCK
    set -l state_dir "$HOME/Library/Application Support/rexurrect"
    set -q REXURRECT_STATE_DIR; and set state_dir $REXURRECT_STATE_DIR
    set -g _rr_foreground_dir "$state_dir/foreground"
    set -g _rr_foreground_id (string replace 'block:' '' -- $REX_BLOCK)
    set -g _rr_foreground_file "$_rr_foreground_dir/$_rr_foreground_id"
    function _rr_foreground_mkdir
        set -l old_umask (umask)
        umask 077
        command mkdir -p -- "$_rr_foreground_dir" 2>/dev/null
        umask $old_umask
    end
    _rr_foreground_mkdir
    function _rr_foreground_preexec --on-event fish_preexec
        string match -q ' *' -- "$argv[1]"; and return 0
        test -d "$_rr_foreground_dir"; or _rr_foreground_mkdir
        set -l tmp "$_rr_foreground_dir/.tmp-$_rr_foreground_id.$fish_pid"
        set -l old_umask (umask)
        umask 077
        begin
            printf '%s' "$argv[1]" >"$tmp"
            and command mv -f -- "$tmp" "$_rr_foreground_file"
        end 2>/dev/null
        umask $old_umask
        return 0
    end
    # A normal exit runs preexec for the exit command but no postexec after it.
    function _rr_foreground_postexec --on-event fish_postexec --on-event fish_exit
        command rm -f -- "$_rr_foreground_file" 2>/dev/null
        return 0
    end
end
