package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/blacktop/rexurrect/internal/autolog"
	"github.com/blacktop/rexurrect/internal/launchagent"
	"github.com/blacktop/rexurrect/internal/rex"
)

// Auto already emitted its result and diagnostic through the bounded writers.
type reportedAutoError struct{ error }

func (e *reportedAutoError) Unwrap() error { return e.error }

func runAgent(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(stdout, "Usage: rexurrect agent <install|enable|disable|uninstall|status> [options]")
		if len(args) == 0 {
			return errors.New("an agent command is required")
		}
		return err
	}
	command := args[0]
	if command != "install" && command != "enable" && command != "disable" && command != "uninstall" && command != "status" {
		return fmt.Errorf("unknown agent command %q", command)
	}
	flags := flag.NewFlagSet("agent "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var c launchagent.Config
	flags.StringVar(&c.Server, "server", "", "explicit unix://PATH endpoint (required for install)")
	flags.StringVar(&c.StateDir, "state-dir", "", "absolute private snapshot directory (required for install)")
	flags.StringVar(&c.Rex, "rex", "rex", "Rex executable path")
	flags.StringVar(&c.Label, "label", launchagent.DefaultLabel, "the one LaunchAgent label to manage")
	flags.DurationVar(&c.Interval, "interval", launchagent.DefaultInterval, "whole-second cadence between 1m and 24h")
	flags.DurationVar(&c.Wait, "wait", rex.DefaultAutoWait, "maximum login-time wait for Rex availability")
	flags.DurationVar(&c.Timeout, "timeout", rex.DefaultAutoTimeout, "overall cycle deadline, at most 1h")
	flags.DurationVar(&c.CallTimeout, "call-timeout", rex.DefaultCallTimeout, "per-attempt Rex deadline, at most 60s")
	flags.Int64Var(&c.LogMaxBytes, "log-max-bytes", autolog.DefaultMaxBytes, "maximum bytes in each automatic log")
	plistDir := flags.String("plist-dir", "", "absolute LaunchAgents directory override for isolated tests")
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("agent commands do not accept positional arguments")
	}
	a, err := launchagent.New(c.Label, *plistDir)
	if err != nil {
		return err
	}
	var status launchagent.Status
	switch command {
	case "install":
		c.Executable, err = os.Executable()
		if err == nil {
			var resolved string
			resolved, err = resolveRex(c.Rex, stderr)
			if err == nil {
				c.ResolvedRex = resolved
				if strings.ContainsRune(c.Rex, filepath.Separator) {
					c.Rex, err = filepath.Abs(c.Rex)
				} else {
					c.Rex = resolved
				}
			}
		}
		if err == nil {
			status, err = a.Install(ctx, c)
		}
	case "enable":
		status, err = a.Enable(ctx)
	case "disable":
		status, err = a.Disable(ctx)
	case "uninstall":
		status, err = a.Uninstall(ctx)
	case "status":
		status, err = a.Status(ctx)
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(status)
	}
	_, err = fmt.Fprintf(stdout, "%s/%s: installed=%t loaded=%t changed=%t\nPlist: %s\n", status.Domain, status.Label, status.Installed, status.Loaded, status.Changed, status.PlistPath)
	return err
}
