package main

import (
	"errors"
	"flag"
	"io"
	"os"

	"github.com/blacktop/rexurrect/internal/foreground"
	"github.com/blacktop/rexurrect/internal/launchagent"
)

// runHook prints an embedded hook so a cask-installed binary needs no
// checkout: the shell recorder for zsh or fish, or the init.lua snippet that
// wakes the LaunchAgent when Rex starts.
func runHook(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: rexurrect hook <zsh|fish|lua> [options]")
	}
	if args[0] != "lua" {
		if len(args) != 1 {
			return errors.New("usage: rexurrect hook <zsh|fish>")
		}
		script, err := foreground.Script(args[0])
		if err != nil {
			return err
		}
		_, err = stdout.Write(script)
		return err
	}
	flags := flag.NewFlagSet("hook lua", flag.ContinueOnError)
	flags.SetOutput(stderr)
	label := flags.String("label", launchagent.DefaultLabel, "the LaunchAgent label the snippet wakes")
	plistDir := flags.String("plist-dir", "", "absolute LaunchAgents directory override for isolated tests")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("hook lua does not accept positional arguments")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	snippet, err := launchagent.LuaHook(executable, *label, *plistDir)
	if err != nil {
		return err
	}
	_, err = stdout.Write(snippet)
	return err
}
