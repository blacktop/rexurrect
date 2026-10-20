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
			continue
		}
		hex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !hex {
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
	if !ValidHarness(r.Harness) {
		return errors.New("receipt harness is not allowlisted")
	}
	return nil
}

// ValidHarness reports whether name is one of the allowlisted account wrappers.
func ValidHarness(name string) bool {
	switch name {
	case "claude", "claude-ddb", "codex", "codex-team", "claude-team":
		return true
	default:
		return false
	}
}

// ClearGuard narrows a clear to the receipt whose fields match every supplied
// guard. An empty field matches any receipt under the key.
type ClearGuard struct {
	Harness      string
	Conversation string
}

// Validate rejects a supplied harness outside the allowlist and a supplied
// conversation that is not a UUID. Empty fields are wildcards and always pass.
func (g ClearGuard) Validate() error {
	if g.Harness != "" && !ValidHarness(g.Harness) {
		return errors.New("receipt harness guard is not allowlisted")
	}
	if g.Conversation != "" && !ValidConversationID(g.Conversation) {
		return errors.New("receipt conversation must be a UUID")
	}
	return nil
}

// Matches reports whether r satisfies every supplied guard field. Conversation
// UUIDs compare case-insensitively because ValidConversationID accepts both
// hex cases.
func (g ClearGuard) Matches(r Receipt) bool {
	return (g.Harness == "" || r.Harness == g.Harness) &&
		(g.Conversation == "" || strings.EqualFold(r.Conversation, g.Conversation))
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
