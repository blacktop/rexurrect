package state

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/snapshot"
)

// Delivery outcomes. A block gets at most one terminal outcome per server
// instance: once its command has been typed or run, nothing is typed again.
const (
	// DeliveryIntent is written before any input reaches the shell, so a
	// lost acknowledgement becomes uncertain instead of a second attempt.
	DeliveryIntent = "intent"
	// DeliveryResumed means the shell ran a receipt's resume command.
	DeliveryResumed = "resumed"
	// DeliveryTypedOnly means the shell put the command at its prompt and
	// left Enter to the user.
	DeliveryTypedOnly = "typed_only"
	// DeliverySkipped means nothing reached the shell; Reason says why.
	DeliverySkipped = "skipped"
	// DeliveryUncertain means input was requested but the shell never
	// acknowledged it; Reason says what was seen.
	DeliveryUncertain = "uncertain"
)

// Delivery kinds: the saved input a block carries.
const (
	DeliveryReceipt    = "receipt"
	DeliveryForeground = "foreground"
)

const (
	maxDeliveryReason = 256
	maxDeliveryShell  = 64
	maxDeliveryInput  = 4096
	deliveryPrefix    = "deliver-"
)

// Delivery is what a restore did about the saved input of one block in a
// session Rex restored itself: its receipt's resume command (kind receipt)
// or its recorded foreground command (kind foreground). Rex brings such a
// pane back as a bare shell, so the input has to be handed to that shell.
type Delivery struct {
	// Generation is the saved generation the input came from.
	Generation uint64            `json:"generation"`
	HostID     string            `json:"host_id"`
	InstanceID string            `json:"instance_id"`
	SessionID  snapshot.SourceID `json:"source_session_id"`
	BlockID    snapshot.SourceID `json:"block_source_id"`
	Kind       string            `json:"kind"`
	// Shell is the shell observed at the pane's prompt, when one was.
	Shell string `json:"shell,omitempty"`
	// Input is the command line without its Enter.
	Input   string `json:"input"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

// Terminal reports whether d ends the block's delivery on this instance.
func (d Delivery) Terminal() bool { return d.Outcome != "" && d.Outcome != DeliveryIntent }

// DeliveryFor is the delivery a block's saved input calls for, before any
// outcome: false when the block has nothing to deliver. A receipt takes
// precedence over a foreground record, as it does for a created pane.
func DeliveryFor(e Envelope, session snapshot.SourceID, b snapshot.Block) (Delivery, bool) {
	input := b.InitialInput()
	if input == "" {
		return Delivery{}, false
	}
	kind := DeliveryForeground
	if b.AgentResume != nil {
		kind = DeliveryReceipt
	}
	return Delivery{Generation: e.Generation, SessionID: session, BlockID: b.ID, Kind: kind, Input: strings.TrimSuffix(input, "\n")}, true
}

// DeliveryLog is the write-ahead log of deliveries on one server instance,
// across every generation restored on it, published atomically like the
// restore journal. Its identity is the host and instance alone, because the
// promise it keeps is per block per instance: a block delivered from one
// generation is never delivered again from a later one.
type DeliveryLog struct {
	FormatVersion int        `json:"format_version"`
	HostID        string     `json:"host_id"`
	InstanceID    string     `json:"instance_id"`
	Records       []Delivery `json:"records"`
}

func NewDeliveryLog(host, instance string) DeliveryLog {
	return DeliveryLog{FormatVersion: FormatVersion, HostID: host, InstanceID: instance, Records: []Delivery{}}
}

func deliveryName(host, instance string) string {
	identity := sha256.Sum256([]byte(host + "\x00" + instance))
	return fmt.Sprintf("%s%x.json", deliveryPrefix, identity)
}

// Latest is the last record of every block.
func (l DeliveryLog) Latest() map[snapshot.SourceID]Delivery {
	out := make(map[snapshot.SourceID]Delivery)
	for _, d := range l.Records {
		out[d.BlockID] = d
	}
	return out
}

// Outcomes lists the latest record of every block in block ID order, nil
// when nothing was recorded, so an empty log reads like an absent one.
func (l DeliveryLog) Outcomes() []Delivery {
	latest := l.Latest()
	if len(latest) == 0 {
		return nil
	}
	out := make([]Delivery, 0, len(latest))
	for _, d := range latest {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Delivery) int { return strings.Compare(string(a.BlockID), string(b.BlockID)) })
	return out
}

// validate checks the log's own consistency. Records may come from
// generations the store no longer holds, so the saved input behind each one
// is checked when it is recorded, not here.
func (l DeliveryLog) validate(host, instance string) error {
	if l.FormatVersion != FormatVersion || l.HostID != host || l.InstanceID != instance || !bounded(host, 256) || !bounded(instance, 256) || l.Records == nil || len(l.Records) > 65536 {
		return errors.New("invalid delivery log binding or record count")
	}
	latest := make(map[snapshot.SourceID]Delivery)
	for _, d := range l.Records {
		if d.Generation == 0 || d.HostID != host || d.InstanceID != instance || !receipt.ValidSessionID(string(d.SessionID)) || !receipt.ValidBlockID(string(d.BlockID)) || d.Kind != DeliveryReceipt && d.Kind != DeliveryForeground || !bounded(d.Input, maxDeliveryInput) || len(d.Shell) > maxDeliveryShell || len(d.Reason) > maxDeliveryReason {
			return errors.New("invalid delivery record")
		}
		previous := latest[d.BlockID]
		if previous.Terminal() {
			return errors.New("delivery recorded after a terminal outcome")
		}
		if previous.Outcome == DeliveryIntent && (previous.Generation != d.Generation || previous.Input != d.Input || previous.Kind != d.Kind) {
			return errors.New("delivery outcome does not match its intent")
		}
		var ok bool
		switch d.Outcome {
		case DeliveryIntent:
			ok = previous.Outcome == "" && d.Reason == ""
		case DeliveryResumed:
			ok = previous.Outcome == DeliveryIntent && d.Reason == "" && d.Kind == DeliveryReceipt
		case DeliveryTypedOnly:
			ok = previous.Outcome == DeliveryIntent && d.Reason == ""
		case DeliverySkipped:
			ok = d.Reason != ""
		case DeliveryUncertain:
			ok = previous.Outcome == DeliveryIntent && d.Reason != ""
		}
		if !ok {
			return errors.New("invalid delivery transition")
		}
		latest[d.BlockID] = d
	}
	return nil
}

// LoadDeliveries returns the delivery log of this server instance, empty
// when no delivery has been recorded on it yet.
func (s *Store) LoadDeliveries(host, instance string) (DeliveryLog, error) {
	var l DeliveryLog
	err := s.read(deliveryName(host, instance), MaxFileSize, &l)
	if errors.Is(err, os.ErrNotExist) {
		return NewDeliveryLog(host, instance), nil
	}
	if err != nil {
		return l, err
	}
	return l, l.validate(host, instance)
}

// saved reports whether d names the saved input of a block in e exactly.
func (d Delivery) saved(e Envelope) bool {
	if e.HostID != d.HostID {
		return false
	}
	for _, session := range e.Snapshot.Sessions {
		if session.ID != d.SessionID {
			continue
		}
		for _, b := range session.Blocks() {
			want, ok := DeliveryFor(e, session.ID, *b)
			if ok && want.BlockID == d.BlockID {
				return want.Kind == d.Kind && want.Input == d.Input && want.Generation == d.Generation
			}
		}
	}
	return false
}

// RecordDelivery binds d to the log, checks it against the saved input of
// its block in e, publishes and fsyncs the whole next log, and only then
// exposes the record to the caller.
func (s *Store) RecordDelivery(e Envelope, l *DeliveryLog, d Delivery) error {
	d.HostID, d.InstanceID = l.HostID, l.InstanceID
	if !d.saved(e) {
		return errors.New("delivery record does not match the saved input")
	}
	next := *l
	next.Records = append(slices.Clone(l.Records), d)
	if err := next.validate(l.HostID, l.InstanceID); err != nil {
		return err
	}
	if err := s.write(deliveryName(l.HostID, l.InstanceID), next); err != nil {
		return err
	}
	*l = next
	return nil
}

// ResolveDelivery closes the intent of block with outcome and reason. The
// intent already carried the saved input, so no generation is consulted:
// an interrupted run is closed as uncertain even after the generation it
// restored has been replaced.
func (s *Store) ResolveDelivery(l *DeliveryLog, block snapshot.SourceID, outcome, reason string) error {
	d := l.Latest()[block]
	if d.Outcome != DeliveryIntent {
		return fmt.Errorf("block %s has no delivery intent to resolve", block)
	}
	d.Outcome, d.Reason = outcome, reason
	next := *l
	next.Records = append(slices.Clone(l.Records), d)
	if err := next.validate(l.HostID, l.InstanceID); err != nil {
		return err
	}
	if err := s.write(deliveryName(l.HostID, l.InstanceID), next); err != nil {
		return err
	}
	*l = next
	return nil
}

// PruneDeliveries removes the delivery logs of every other server instance:
// a log only matters while its instance is the live one.
func (s *Store) PruneDeliveries(host, instance string) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	keep := deliveryName(host, instance)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == keep || !strings.HasPrefix(name, deliveryPrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
			return err
		}
	}
	return nil
}
