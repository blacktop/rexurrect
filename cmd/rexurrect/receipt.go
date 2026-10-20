package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/rex"
)

func runReceipt(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("receipt requires set, clear, or list")
	}
	action := args[0]
	if action != "set" && action != "clear" && action != "list" {
		return fmt.Errorf("unknown receipt command %q", action)
	}
	flags := flag.NewFlagSet("receipt "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", "", "required explicit unix://PATH endpoint")
	dir := flags.String("state-dir", "", "required private snapshot directory")
	binary := flags.String("rex", "rex", "Rex executable path")
	callTimeout := flags.Duration("call-timeout", rex.DefaultCallTimeout, "per-attempt Rex deadline, at most 60s")
	lockTimeout := rex.DefaultLockTimeout
	var r receipt.Receipt
	var guard receipt.ClearGuard
	var jsonOutput bool
	flags.StringVar(&r.SessionID, "session", "", "full Rex session ID (optional filter for list)")
	flags.StringVar(&r.BlockID, "block", "", "full Rex block ID (optional filter for list)")
	if action == "set" || action == "clear" {
		flags.DurationVar(&lockTimeout, "lock-timeout", rex.DefaultLockTimeout, "state lock wait, at most 60s")
	}
	if action == "set" {
		flags.StringVar(&r.Conversation, "conversation", "", "conversation UUID")
		flags.StringVar(&r.Harness, "harness", "", "claude, claude-ddb, codex, codex-team, or claude-team")
		flags.StringVar(&r.Model, "model", "", "optional model token")
		flags.StringVar(&r.Effort, "effort", "", "optional low, medium, high, or xhigh")
	}
	if action == "clear" {
		flags.StringVar(&guard.Conversation, "conversation", "", "optional conversation UUID guard")
		flags.StringVar(&guard.Harness, "harness", "", "optional harness guard; with --conversation both must match")
	}
	if action == "list" {
		flags.BoolVar(&jsonOutput, "json", false, "print a JSON array")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	socket, err := rex.SocketPath(*server)
	if err != nil {
		return err
	}
	if *dir == "" || *binary == "" || flags.NArg() != 0 {
		return errors.New("--state-dir and --rex must be nonempty, and positional arguments are not accepted")
	}
	if err := rex.ValidateCallTimeout(*callTimeout); err != nil {
		return err
	}
	if err := rex.ValidateLockTimeout(lockTimeout); err != nil {
		return err
	}
	resolved, err := resolveRex(*binary, stderr)
	if err != nil {
		return err
	}
	c := rex.Client{Binary: resolved, Socket: socket, CallTimeout: *callTimeout, LockTimeout: lockTimeout}
	switch action {
	case "set":
		return c.SetReceipt(ctx, *dir, r)
	case "clear":
		return c.ClearReceipt(ctx, *dir, r.Key, guard)
	default:
		// Filters use the same strict ID grammar as registration.
		if r.SessionID != "" && !receipt.ValidSessionID(r.SessionID) || r.BlockID != "" && !receipt.ValidBlockID(r.BlockID) {
			return errors.New("receipt list filters require full Rex IDs")
		}
		entries, err := c.ListReceipts(ctx, *dir)
		if err != nil {
			return err
		}
		filtered := make([]receipt.Receipt, 0, len(entries))
		for _, entry := range entries {
			if (r.SessionID == "" || entry.SessionID == r.SessionID) && (r.BlockID == "" || entry.BlockID == r.BlockID) {
				filtered = append(filtered, entry)
			}
		}
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(filtered)
		}
		for _, entry := range filtered {
			if _, err := fmt.Fprintf(stdout, "%s %s %s %s model=%s effort=%s\n", entry.SessionID, entry.BlockID, entry.Harness, entry.Conversation, entry.Model, entry.Effort); err != nil {
				return err
			}
		}
		return nil
	}
}
