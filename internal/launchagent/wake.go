package launchagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
)

// WakeResult reports what the startup trigger did. A skip is a benign outcome:
// the timer and WatchPaths cycles of the ordinary job remain the fallback.
type WakeResult struct {
	Label     string `json:"label"`
	Domain    string `json:"domain"`
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	ParentPID int    `json:"parent_pid"`
	ServerPID int    `json:"server_pid,omitempty"`
}

// Wake runs the owned job early when the calling process was started by the
// Rex server the job is configured for. serverPID answers with the PID of the
// server behind the owned configuration once it is reachable, or 0 when it
// gave up. launchctl is not consulted before the identity check, so a config
// check, a reload of another server, or a shell cannot dispatch recovery.
func (a *Agent) Wake(ctx context.Context, parentPID int, parentAlive func() bool, serverPID func(context.Context, Config) (int, error)) (WakeResult, error) {
	r := WakeResult{Label: a.Label, Domain: a.Domain, Outcome: "skipped", ParentPID: parentPID}
	if parentPID <= 1 || !parentAlive() {
		r.Reason = "the parent process already exited; not started by a running Rex server"
		return r, nil
	}
	owned, values, err := a.readOwned()
	if errors.Is(err, os.ErrNotExist) {
		r.Reason = "no owned LaunchAgent plist; nothing to wake"
		return r, nil
	}
	if err != nil {
		return r, err
	}
	c, err := parseConfig(values)
	if err != nil {
		return r, err
	}
	pid, err := serverPID(ctx, c)
	if err != nil {
		return r, err
	}
	r.ServerPID = pid
	if pid <= 0 {
		r.Reason = "the configured Rex server did not answer before the parent exited or the wait ended"
		return r, nil
	}
	if pid != parentPID {
		r.Reason = fmt.Sprintf("parent PID %d is not the configured Rex server (PID %d)", parentPID, pid)
		return r, nil
	}
	r.Reason, err = a.wakeGuard(ctx, c, values["ProgramArguments"].Strings, values["Disabled"])
	if err != nil || r.Reason != "" {
		return r, err
	}
	// The endpoint wait and launchd checks can outlive a disable, uninstall,
	// reinstall, or the parent itself. Do not dispatch from stale authority.
	current, _, err := a.readOwned()
	if errors.Is(err, os.ErrNotExist) || err == nil && !bytes.Equal(current, owned) {
		r.Reason = "the owned LaunchAgent plist changed while waiting"
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if !parentAlive() {
		r.Reason = "the parent process exited while waiting"
		return r, nil
	}
	started, err := a.kickstart(ctx)
	if err != nil {
		return r, err
	}
	if !started {
		r.Reason = "the LaunchAgent is not loaded (disabled or uninstalled)"
		return r, nil
	}
	r.Outcome = "woken"
	return r, nil
}
