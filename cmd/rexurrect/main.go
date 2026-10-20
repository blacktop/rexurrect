package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/blacktop/rexurrect/internal/autolog"
	"github.com/blacktop/rexurrect/internal/rex"
	"github.com/blacktop/rexurrect/internal/rexbin"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

var version = "dev"

func reportRexSubstitution(stderr io.Writer, sub *rexbin.Substitution) error {
	if sub == nil {
		return nil
	}
	_, err := fmt.Fprintf(stderr, "Rex executable %q unavailable; substituted %q\n", sub.Configured, sub.Resolved)
	return err
}

func resolveRex(binary string, stderr io.Writer) (string, error) {
	resolved, sub, err := rexbin.Resolve(binary)
	if err != nil {
		return "", err
	}
	return resolved, reportRexSubstitution(stderr, sub)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if _, ok := errors.AsType[*reportedAutoError](err); !ok {
			fmt.Fprintln(os.Stderr, "rexurrect:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(stdout, "Usage: rexurrect <save|restore|status|auto> --server unix://PATH --state-dir DIR [options]\n       rexurrect agent <install|enable|disable|uninstall|status> [options]\n       rexurrect receipt <set|clear|list> --server unix://PATH --state-dir DIR [options]\n       rexurrect version")
		if len(args) == 0 {
			return errors.New("a command is required")
		}
		return err
	}
	command := args[0]
	if command == "version" || command == "--version" {
		if len(args) != 1 {
			return errors.New("version does not accept arguments")
		}
		_, err := fmt.Fprintln(stdout, "rexurrect", version)
		return err
	}
	if command == "agent" {
		return runAgent(ctx, args[1:], stdout, stderr)
	}
	if command == "receipt" {
		return runReceipt(ctx, args[1:], stdout, stderr)
	}
	if command != "save" && command != "status" && command != "restore" && command != "auto" {
		return fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", "", "required explicit unix://PATH endpoint")
	dir := flags.String("state-dir", "", "required private snapshot directory")
	binary := flags.String("rex", "rex", "Rex executable path")
	callTimeout := flags.Duration("call-timeout", rex.DefaultCallTimeout, "per-attempt Rex deadline, at most 60s")
	var allowEmpty, jsonOutput bool
	var generation uint64
	var wait, timeout time.Duration
	var logMaxBytes int64
	if command == "save" {
		flags.BoolVar(&allowEmpty, "allow-empty", false, "allow replacing nonempty saved state with an empty server")
	} else {
		flags.BoolVar(&jsonOutput, "json", false, "print machine-readable JSON")
	}
	if command == "restore" {
		flags.Uint64Var(&generation, "generation", 0, "saved generation number (default: current)")
	}
	if command == "auto" {
		flags.DurationVar(&wait, "wait", rex.DefaultAutoWait, "maximum wait for Rex availability")
		flags.DurationVar(&timeout, "timeout", rex.DefaultAutoTimeout, "overall automatic-cycle deadline")
		flags.Int64Var(&logMaxBytes, "log-max-bytes", autolog.DefaultMaxBytes, "maximum bytes in each automatic LaunchAgent log")
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
	client := rex.Client{Binary: *binary, Socket: socket, CallTimeout: *callTimeout}
	if command == "auto" {
		var err error
		stdout, err = autolog.Wrap(stdout, filepath.Join(*dir, autolog.OutName), logMaxBytes)
		if err != nil {
			return err
		}
		stderr, err = autolog.Wrap(stderr, filepath.Join(*dir, autolog.ErrName), logMaxBytes)
		if err != nil {
			return err
		}
		result, cycleErr := client.AutoCycle(ctx, *dir, wait, timeout)
		var textErr error
		if cycleErr != nil {
			_, textErr = fmt.Fprintf(stderr, "Automatic cycle %s: %s\n", result.State, result.Reason)
		} else if result.Reason == "unchanged" {
			_, textErr = fmt.Fprintf(stderr, "Automatic cycle %s: unchanged generation %d (instance %s, restore %s)\n", result.State, result.Generation, result.InstanceID, result.RestoreOutcome)
		} else {
			_, textErr = fmt.Fprintf(stderr, "Automatic cycle %s: saved generation %d (instance %s, restore %s)\n", result.State, result.SavedGeneration, result.InstanceID, result.RestoreOutcome)
		}
		textErr = errors.Join(textErr, reportRexSubstitution(stderr, result.RexBinarySubstitution))
		for _, sub := range result.CaptureCWDSubstitutions {
			_, err := fmt.Fprintf(stderr, "Block %s: no cwd; substituted %q during capture\n", sub.BlockID, sub.CWD)
			textErr = errors.Join(textErr, err)
		}
		if err := errors.Join(cycleErr, textErr, json.NewEncoder(stdout).Encode(result)); err != nil {
			return &reportedAutoError{err}
		}
		return nil
	}
	client.Binary, err = resolveRex(client.Binary, stderr)
	if err != nil {
		return err
	}
	if command == "save" {
		saved, err := client.Save(ctx, *dir, allowEmpty)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Saved generation %d (%d sessions)\n", saved.Generation, len(saved.Snapshot.Sessions))
		for _, sub := range saved.Report.CWDSubstitutions {
			_, printErr := fmt.Fprintf(stdout, "Block %s: no cwd; substituted %q during capture\n", sub.BlockID, sub.CWD)
			err = errors.Join(err, printErr)
		}
		for _, id := range saved.Report.PrunedReceipts {
			_, printErr := fmt.Fprintf(stdout, "Pruned receipt %s\n", id)
			err = errors.Join(err, printErr)
		}
		return err
	}
	if command == "restore" {
		result, restoreErr := client.RestoreSaved(ctx, *dir, generation)
		if jsonOutput {
			return errors.Join(restoreErr, json.NewEncoder(stdout).Encode(result))
		}
		if result.Generation == 0 {
			return restoreErr
		}
		_, printErr := fmt.Fprintf(stdout, "Generation %d: journal %s (instance %s)\n", result.Generation, result.Outcome, result.InstanceID)
		keys := make([]string, 0, len(result.IDMap))
		for source := range result.IDMap {
			keys = append(keys, string(source))
		}
		slices.Sort(keys)
		for _, source := range keys {
			_, err := fmt.Fprintf(stdout, "%s -> %s\n", source, result.IDMap[snapshot.SourceID(source)])
			printErr = errors.Join(printErr, err)
		}
		for _, sub := range result.CWDSubstitutions {
			_, err := fmt.Fprintf(stdout, "Block %s: missing cwd %q; substituted %q\n", sub.BlockID, sub.SavedCWD, sub.RestoredCWD)
			printErr = errors.Join(printErr, err)
		}
		for _, skip := range result.NotResumed {
			_, err := fmt.Fprintf(stdout, "Block %s: %s not resumed (%s)\n", skip.BlockID, skip.Harness, skip.Reason)
			printErr = errors.Join(printErr, err)
		}
		return errors.Join(restoreErr, printErr)
	}
	status, err := client.Status(ctx, *dir)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(stdout).Encode(status)
	}
	_, err = fmt.Fprintf(stdout, "Generation %d (last-known-good %d)\nServer %s\nHost %s, captured instance %s\nRex %s (%s)\n%d sessions, %d windows, %d blocks, %d unsupported\nJournal %s (instance %s)\n", status.Generation, status.LastKnownGood, status.Server, status.HostID, status.InstanceID, status.RexVersion, status.Commit, status.Sessions, status.Windows, status.Blocks, status.Unsupported, status.Journal.Outcome, status.Journal.InstanceID)
	for _, id := range status.Journal.MissingSessions {
		_, printErr := fmt.Fprintf(stdout, "Missing completed session %s\n", id)
		err = errors.Join(err, printErr)
	}
	return err
}
