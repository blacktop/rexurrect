package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

var ErrNoJournal = errors.New("restore journal does not exist")

const markerPrefix = "rexurrect:"

// Keep the original wire prefix for journals created before the rename.
const legacyMarkerPrefix = "rex-" + "resurrect:"

// Journal is an ordered, atomically published write-ahead log. Every record
// repeats its binding so a misplaced or spliced record fails validation.
type Journal struct {
	FormatVersion int             `json:"format_version"`
	Generation    uint64          `json:"generation"`
	HostID        string          `json:"host_id"`
	InstanceID    string          `json:"instance_id"`
	Records       []JournalRecord `json:"records"`
}

type JournalRecord struct {
	Generation       uint64            `json:"generation"`
	HostID           string            `json:"host_id"`
	InstanceID       string            `json:"instance_id"`
	SourceID         snapshot.SourceID `json:"source_session_id"`
	Marker           string            `json:"operation_marker"`
	Outcome          string            `json:"outcome"`
	IDMap            snapshot.IDMap    `json:"id_map,omitempty"`
	CWDSubstitutions []CWDSubstitution `json:"cwd_substitutions,omitempty"`
	NotResumed       []AgentNotResumed `json:"not_resumed,omitempty"`
}

// CWDSubstitution is a destination chosen before a session create. Keeping it
// in the intent makes lost-ack recovery independent of later filesystem changes.
type CWDSubstitution struct {
	BlockID     snapshot.SourceID `json:"block_source_id"`
	SavedCWD    string            `json:"saved_cwd"`
	RestoredCWD string            `json:"restored_cwd"`
}

type JournalStatus struct {
	Generation       uint64            `json:"generation"`
	HostID           string            `json:"host_id"`
	InstanceID       string            `json:"instance_id"`
	Outcome          string            `json:"outcome"`
	IDMap            snapshot.IDMap    `json:"id_map"`
	MissingSessions  []snapshot.LiveID `json:"missing_sessions,omitempty"`
	CWDSubstitutions []CWDSubstitution `json:"cwd_substitutions,omitempty"`
	NotResumed       []AgentNotResumed `json:"not_resumed,omitempty"`
}

func NewOperationMarker() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return markerPrefix + hex.EncodeToString(entropy[:]), nil
}

func NewJournal(e Envelope, host, instance string) Journal {
	return Journal{FormatVersion: FormatVersion, Generation: e.Generation, HostID: host, InstanceID: instance, Records: []JournalRecord{}}
}

func journalName(generation uint64, host, instance string) string {
	identity := sha256.Sum256([]byte(host + "\x00" + instance))
	return fmt.Sprintf("restore-%020d-%x.json", generation, identity)
}

// LoadGeneration also validates the current pointer, so an unreferenced
// generation left by interrupted publication cannot be selected for restore.
func (s *Store) LoadGeneration(n uint64) (Envelope, error) {
	current, err := s.LoadCurrent()
	if err != nil {
		return Envelope{}, err
	}
	if n == 0 || n == current.Envelope.Generation {
		return current.Envelope, nil
	}
	if n > current.Envelope.Generation {
		return Envelope{}, errors.New("requested generation is newer than current")
	}
	return s.load(n)
}

func (j Journal) Latest() map[snapshot.SourceID]JournalRecord {
	out := make(map[snapshot.SourceID]JournalRecord)
	for _, record := range j.Records {
		out[record.SourceID] = record
	}
	return out
}

func (j Journal) Status(e Envelope) JournalStatus {
	out := JournalStatus{Generation: j.Generation, HostID: j.HostID, InstanceID: j.InstanceID, Outcome: "complete", IDMap: make(snapshot.IDMap)}
	for _, r := range j.Records {
		out.CWDSubstitutions = append(out.CWDSubstitutions, r.CWDSubstitutions...)
		out.NotResumed = append(out.NotResumed, r.NotResumed...)
	}
	latest := j.Latest()
	for _, session := range e.Snapshot.Sessions {
		r := latest[session.ID]
		if r.Outcome == "intent" || r.Outcome == "uncertain" {
			out.Outcome = "uncertain"
		} else if r.Outcome != "complete" && out.Outcome != "uncertain" {
			out.Outcome = "pending"
		}
		for old, live := range r.IDMap {
			out.IDMap[old] = live
		}
	}
	return out
}

func (j Journal) validate(e Envelope, host, instance string) error {
	if j.FormatVersion != FormatVersion || j.Generation == 0 || j.Generation != e.Generation || j.HostID != e.HostID || j.HostID != host || j.InstanceID != instance || !bounded(host, 256) || !bounded(instance, 256) || j.Records == nil || len(j.Records) > 6*len(e.Snapshot.Sessions) {
		return errors.New("invalid restore journal binding or record count")
	}
	inventories := make(map[snapshot.SourceID]map[snapshot.SourceID]string)
	skips := make(map[snapshot.SourceID][]AgentNotResumed)
	cwds := make(map[snapshot.SourceID]string)
	for _, session := range e.Snapshot.Sessions {
		ids, err := (snapshot.Snapshot{Version: snapshot.Version, Sessions: []snapshot.Session{session}}).Inventory()
		if err != nil {
			return err
		}
		inventories[session.ID] = ids
		skips[session.ID] = agentNotResumed(session)
		for _, b := range session.Blocks() {
			cwds[b.ID] = b.CWD
		}
	}
	latest := make(map[snapshot.SourceID]JournalRecord)
	markers := make(map[string]snapshot.SourceID)
	liveIDs := make(map[snapshot.LiveID]snapshot.SourceID)
	for _, r := range j.Records {
		inventory, ok := inventories[r.SourceID]
		encoded, hasPrefix := strings.CutPrefix(r.Marker, markerPrefix)
		if !hasPrefix {
			encoded, hasPrefix = strings.CutPrefix(r.Marker, legacyMarkerPrefix)
		}
		entropy, err := hex.DecodeString(encoded)
		if !ok || r.Generation != j.Generation || r.HostID != host || r.InstanceID != instance || !hasPrefix || err != nil || len(entropy) != 16 {
			return errors.New("invalid restore journal record")
		}
		previous := latest[r.SourceID]
		if r.Outcome == "intent" && !slices.Equal(r.NotResumed, skips[r.SourceID]) || r.Outcome != "intent" && len(r.NotResumed) != 0 {
			return errors.New("invalid not-resumed journal entries")
		}
		allowed := false
		switch r.Outcome {
		case "intent":
			allowed = previous.Outcome == "" && len(r.IDMap) == 0
		case "uncertain":
			allowed = previous.Outcome == "intent" && len(r.IDMap) == 0
		case "created":
			allowed = previous.Outcome == "intent"
		case "recovered":
			allowed = previous.Outcome == "intent" || previous.Outcome == "uncertain"
		case "configuring":
			allowed = previous.Outcome == "created" || previous.Outcome == "recovered"
		case "complete":
			allowed = previous.Outcome == "configuring"
		}
		if !allowed || previous.Marker != "" && previous.Marker != r.Marker || markers[r.Marker] != "" && markers[r.Marker] != r.SourceID {
			return errors.New("invalid restore journal transition or marker")
		}
		if len(r.CWDSubstitutions) > len(inventory) || len(r.CWDSubstitutions) != 0 && r.Outcome != "intent" {
			return errors.New("invalid restore cwd substitutions")
		}
		seenCWD := make(map[snapshot.SourceID]bool, len(r.CWDSubstitutions))
		for _, sub := range r.CWDSubstitutions {
			if inventory[sub.BlockID] != "block" || seenCWD[sub.BlockID] || sub.SavedCWD != cwds[sub.BlockID] || sub.RestoredCWD == sub.SavedCWD || !filepath.IsAbs(sub.RestoredCWD) || filepath.Clean(sub.RestoredCWD) != sub.RestoredCWD || len(sub.RestoredCWD) > 4096 || strings.ContainsRune(sub.RestoredCWD, 0) {
				return errors.New("invalid restore cwd substitution")
			}
			seenCWD[sub.BlockID] = true
		}
		if r.Outcome != "intent" && r.Outcome != "uncertain" && r.IDMap[r.SourceID] == "" {
			return errors.New("restore journal lost its acknowledged session")
		}
		if (r.Outcome == "configuring" || r.Outcome == "complete") && len(r.IDMap) != len(inventory) {
			return errors.New("incomplete restore journal ID map")
		}
		for old, live := range previous.IDMap {
			if r.IDMap[old] != live {
				return errors.New("restore journal changed an acknowledged mapping")
			}
		}
		for old, live := range r.IDMap {
			if inventory[old] == "" || !bounded(string(live), 256) || liveIDs[live] != "" && liveIDs[live] != old {
				return errors.New("invalid or duplicate restore journal mapping")
			}
			liveIDs[live] = old
		}
		markers[r.Marker], latest[r.SourceID] = r.SourceID, r
	}
	return nil
}

func (s *Store) LoadJournal(e Envelope, host, instance string) (Journal, error) {
	var j Journal
	if err := s.read(journalName(e.Generation, host, instance), MaxFileSize, &j); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return j, ErrNoJournal
		}
		return j, err
	}
	return j, j.validate(e, host, instance)
}

func (s *Store) WriteJournal(e Envelope, j Journal) error {
	if err := j.validate(e, j.HostID, j.InstanceID); err != nil {
		return err
	}
	return s.write(journalName(j.Generation, j.HostID, j.InstanceID), j)
}

// Record publishes and fsyncs the entire next log before exposing the record
// to the caller. A failed publication never authorizes a server mutation.
func (s *Store) Record(e Envelope, j *Journal, source snapshot.SourceID, marker, outcome string, ids snapshot.IDMap, substitutions ...CWDSubstitution) error {
	var skips []AgentNotResumed
	if outcome == "intent" {
		for _, session := range e.Snapshot.Sessions {
			if session.ID == source {
				skips = agentNotResumed(session)
				break
			}
		}
	}
	next := *j
	next.Records = append(append([]JournalRecord(nil), j.Records...), JournalRecord{Generation: j.Generation, HostID: j.HostID, InstanceID: j.InstanceID, SourceID: source, Marker: marker, Outcome: outcome, IDMap: maps.Clone(ids), CWDSubstitutions: append([]CWDSubstitution(nil), substitutions...), NotResumed: skips})
	if err := s.WriteJournal(e, next); err != nil {
		return err
	}
	*j = next
	return nil
}
