// Package receipt defines the only agent inputs allowed in a restored terminal.
package receipt

import (
	"errors"
	"regexp"
	"strings"
)

const MaxSize = 1024

var modelPattern = regexp.MustCompile(`^[a-z][a-z0-9]*([.-][a-z0-9]+)*$`)

type Key struct {
	SessionID string `json:"session_id"`
	BlockID   string `json:"block_id"`
}

type Receipt struct {
	Key
	Harness      string `json:"harness"`
	Conversation string `json:"conversation"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
}

func uuid(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func ValidSessionID(id string) bool      { return validRexID(id, "session:") }
func ValidBlockID(id string) bool        { return validRexID(id, "block:") }
func ValidConversationID(id string) bool { return uuid(id) }

func validRexID(id, prefix string) bool {
	value, ok := strings.CutPrefix(id, prefix)
	return ok && uuid(value)
}

func (k Key) Validate() error {
	if !ValidSessionID(k.SessionID) || !ValidBlockID(k.BlockID) {
		return errors.New("receipt requires full Rex session and block IDs")
	}
	return nil
}

func (r Receipt) Validate() error {
	if err := r.ValidateMetadata(); err != nil {
		return err
	}
	switch r.Harness {
	case "claude", "claude-ddb", "codex", "codex-team", "claude-team":
		return nil
	default:
		return errors.New("receipt harness is not allowlisted")
	}
}

// ValidateMetadata accepts bounded future harness names in saved snapshots.
// Registration and command construction still require Validate's allowlist.
func (r Receipt) ValidateMetadata() error {
	if err := r.Key.Validate(); err != nil {
		return err
	}
	if len(r.Harness) > 128 || !modelPattern.MatchString(r.Harness) {
		return errors.New("invalid receipt harness token")
	}
	if !ValidConversationID(r.Conversation) {
		return errors.New("receipt conversation must be a UUID")
	}
	if len(r.Model) > 128 || r.Model != "" && !modelPattern.MatchString(r.Model) {
		return errors.New("receipt model must match [a-z][a-z0-9]*([.-][a-z0-9]+)* (at most 128 bytes)")
	}
	switch r.Effort {
	case "", "low", "medium", "high", "xhigh":
	default:
		return errors.New("receipt effort must be low, medium, high, or xhigh")
	}
	return nil
}

// InitialInput is constructed only from validated tokens and fixed syntax.
// An empty result means no bytes may be sent, including for claude-team.
func (r Receipt) InitialInput() string {
	if r.Validate() != nil || r.Harness == "claude-team" {
		return ""
	}
	var command string
	switch r.Harness {
	case "claude", "claude-ddb":
		command = r.Harness + " --resume " + r.Conversation
		if r.Model != "" {
			command += " --model " + r.Model
		}
		if r.Effort != "" {
			command += " --effort " + r.Effort
		}
	case "codex", "codex-team":
		command = r.Harness + " resume " + r.Conversation
		if r.Model != "" {
			command += " -m " + r.Model
		}
		if r.Effort != "" {
			command += ` -c model_reasoning_effort="` + r.Effort + `"`
		}
	default:
		return ""
	}
	return command + "\n"
}
