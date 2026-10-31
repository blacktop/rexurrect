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
	"strings"
	"syscall"
	"time"

	"github.com/blacktop/rexurrect/internal/autolog"
	"github.com/blacktop/rexurrect/internal/rex"
	"github.com/blacktop/rexurrect/internal/rexbin"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

var version = "dev"

// rexAPIText words the advertised API range; 0 means the server left the
// field out.
func rexAPIText(api rex.RexAPI) string {
	return fmt.Sprintf("Rex API revision %d (minimum %d)", api.ServerAPIRevision, api.ServerAPIMinRevision)
}

func recoveryText(r snapshot.Recovery) string {
	return fmt.Sprintf("%d agents resume, %d commands typed, %d plain shells, %d inert receipts", r.Agents, r.Typed, r.Plain, r.Inert)
}

// printNative names the sessions Rex restored itself, which the journal kept
// instead of creating; nothing is printed when there are none.
func printNative(w io.Writer, n int) error {
	if n == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "%d sessions kept from Rex's own restore\n", n)
	return err
}

func deliveryKind(kind string) string {
	if kind == state.DeliveryReceipt {
		return "resume command"
	}
	return "foreground command"
}

// printDeliveries says what became of each saved command in a pane Rex
// restored itself: run, typed and awaiting Enter, not delivered, or unknown.
func printDeliveries(w io.Writer, deliveries []state.Delivery) error {
	var err error
	for _, d := range deliveries {
		kind := deliveryKind(d.Kind)
		var line string
		switch d.Outcome {
		case state.DeliveryResumed:
			line = fmt.Sprintf("Block %s: %s resumed", d.BlockID, kind)
		case state.DeliveryTypedOnly:
			line = fmt.Sprintf("Block %s: %s typed, awaiting Enter", d.BlockID, kind)
		case state.DeliverySkipped:
			line = fmt.Sprintf("Block %s: %s not delivered (%s)", d.BlockID, kind, d.Reason)
		case state.DeliveryUncertain:
			line = fmt.Sprintf("Block %s: %s delivery uncertain (%s)", d.BlockID, kind, d.Reason)
		default:
			line = fmt.Sprintf("Block %s: %s delivery in progress", d.BlockID, kind)
		}
		_, printErr := fmt.Fprintln(w, line)
		err = errors.Join(err, printErr)
	}
	return err
}

// deliverySummary counts deliveries by outcome for one-line reports; empty
// when there were none.
func deliverySummary(deliveries []state.Delivery) string {
	counts := make(map[string]int)
	for _, d := range deliveries {
		counts[d.Outcome]++
	}
	var parts []string
	for _, item := range []struct{ outcome, label string }{{state.DeliveryResumed, "resumed"}, {state.DeliveryTypedOnly, "awaiting Enter"}, {state.DeliverySkipped, "not delivered"}, {state.DeliveryUncertain, "uncertain"}, {state.DeliveryIntent, "in progress"}} {
		if n := counts[item.outcome]; n != 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, item.label))
		}
	}
	return strings.Join(parts, ", ")
}

// printSkips lists the blocks an optional feature skipped, so a manual save
// or restore never looks fully successful when it was not.
func printSkips(w io.Writer, what string, skips []snapshot.Skip) error {
	var err error
	for _, skip := range skips {
		_, printErr := fmt.Fprintf(w, "Block %s: %s (%s)\n", skip.BlockID, what, skip.Reason)
		err = errors.Join(err, printErr)
	}
	return err
}

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
		_, err := fmt.Fprintln(stdout, "Usage: rexurrect <save|restore|status|auto> --server unix://PATH --state-dir DIR [options]\n       rexurrect agent <install|enable|disable|uninstall|status|wake> [options]\n       rexurrect receipt <set|clear|list> --server unix://PATH --state-dir DIR [options]\n       rexurrect scrollback replay --state-dir DIR --key-id ID --object-id ID --host ID --block ID\n       rexurrect hook <zsh|fish|lua> [options]\n       rexurrect version")
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
	if command == "scrollback" {
		return runScrollback(ctx, args[1:], stdout, stderr)
	}
	if command == "hook" {
		return runHook(args[1:], stdout, stderr)
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
	var captureScrollback bool
	if command == "save" || command == "auto" {
		flags.BoolVar(&captureScrollback, "scrollback", false, "save encrypted terminal scrollback (key in the login keychain)")
	}
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
	client := rex.Client{Binary: *binary, Socket: socket, CallTimeout: *callTimeout, Scrollback: captureScrollback, Keychain: keychainStore()}
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
		} else {
			kind, generation := "saved", result.SavedGeneration
			if result.Reason == "unchanged" {
				kind, generation = "unchanged", result.Generation
			}
			detail := fmt.Sprintf("instance %s, restore %s", result.InstanceID, result.RestoreOutcome)
			if result.Restored != (snapshot.Recovery{}) {
				detail += ", " + recoveryText(result.Restored)
			}
			if result.Native != 0 {
				detail += fmt.Sprintf(", %d kept from Rex's own restore", result.Native)
			}
			if summary := deliverySummary(result.Deliveries); summary != "" {
				detail += ", " + summary
			}
			_, textErr = fmt.Fprintf(stderr, "Automatic cycle %s: %s generation %d (%s, %s)\n", result.State, kind, generation, detail, rexAPIText(result.RexAPI))
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
		err = errors.Join(err, printSkips(stdout, "scrollback not captured", saved.Report.ScrollbackSkipped))
		return errors.Join(err, printSkips(stdout, "foreground not recorded", saved.Report.ForegroundSkipped))
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
		printErr = errors.Join(printErr, printNative(stdout, result.Native))
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
		printErr = errors.Join(printErr, printSkips(stdout, "scrollback not replayed", result.ScrollbackSkipped))
		printErr = errors.Join(printErr, printDeliveries(stdout, result.Deliveries))
		return errors.Join(restoreErr, printErr)
	}
	status, err := client.Status(ctx, *dir)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(stdout).Encode(status)
	}
	_, err = fmt.Fprintf(stdout, "Generation %d (last-known-good %d)\nServer %s\nHost %s, captured instance %s\nRex %s (%s)\n%d sessions, %d windows, %d blocks, %d unsupported\nRecovery: %s\n%s\nJournal %s (instance %s)\n", status.Generation, status.LastKnownGood, status.Server, status.HostID, status.InstanceID, status.RexVersion, status.Commit, status.Sessions, status.Windows, status.Blocks, status.Unsupported, recoveryText(status.Recovery), rexAPIText(status.RexAPI), status.Journal.Outcome, status.Journal.InstanceID)
	err = errors.Join(err, printNative(stdout, status.Journal.Native))
	err = errors.Join(err, printDeliveries(stdout, status.Journal.Deliveries))
	for _, id := range status.Journal.MissingSessions {
		_, printErr := fmt.Fprintf(stdout, "Missing completed session %s\n", id)
		err = errors.Join(err, printErr)
	}
	return err
}
