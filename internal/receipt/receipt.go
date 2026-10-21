// Package receipt defines the only agent inputs allowed in a restored terminal.
package receipt

import (
	"errors"
	"regexp"
	"strings"
)

const MaxSize = 1024

var (
	modelPattern = regexp.MustCompile(`^[a-z][a-z0-9]*([.-][a-z0-9]+)*$`)
	// A command name is one shell word: no spaces, shell metacharacters, or
	// leading dash. ValidCommand also excludes reserved shell words.
	commandPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
)

type Key struct {
	SessionID string `json:"session_id"`
	BlockID   string `json:"block_id"`
}

// Receipt has two wire forms. New registrations carry an explicit agent type
// in Host and the exact command name in Command. Receipts written before
// store format 2 carry a legacy Harness instead; their execution semantics are
// frozen so that an upgrade never makes a previously inert receipt executable.
// A receipt that mixes the two forms, or has one half of the new form, is
// invalid.
type Receipt struct {
	Key
	Host         string `json:"host,omitempty"`
	Command      string `json:"command,omitempty"`
	Harness      string `json:"harness,omitempty"`
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

// ValidHost reports whether host names an agent type rexurrect can replay.
func ValidHost(host string) bool { return host == "codex" || host == "claude" }

// ValidCommand reports whether name is a bare command word safe to type in
// bash, zsh, or fish. Quoting does not disable keyword parsing in fish.
func ValidCommand(name string) bool {
	if !commandPattern.MatchString(name) {
		return false
	}
	switch name {
	case "_", "and", "argparse", "begin", "break", "builtin", "case", "command",
		"continue", "coproc", "declare", "do", "done", "elif", "else", "end", "esac",
		"eval", "exec", "export", "fi", "float", "for", "foreach", "function", "if",
		"in", "integer", "local", "nocorrect", "not", "or", "read", "readonly",
		"repeat", "return", "select", "set", "status", "string", "switch", "test",
		"then", "time", "typeset", "until", "while":
		return false
	default:
		return true
	}
}

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

// Legacy reports whether r uses the pre-format-2 harness form.
func (r Receipt) Legacy() bool { return r.Harness != "" }

// Name is the command rexurrect types to resume r, in either form.
func (r Receipt) Name() string {
	if r.Legacy() {
		return r.Harness
	}
	return r.Command
}

// Validate accepts the receipts the store may hold: the new host and command
// form, or a legacy harness from the fixed set that store format 1 allowed.
func (r Receipt) Validate() error {
	if err := r.ValidateMetadata(); err != nil {
		return err
	}
	if r.Legacy() && !legacyHarness(r.Harness) {
		return errors.New("legacy receipt harness is not allowlisted")
	}
	return nil
}

func legacyHarness(name string) bool {
	switch name {
	case "claude", "claude-ddb", "codex", "codex-team", "claude-team":
		return true
	default:
		return false
	}
}

// ValidateMetadata is Validate without the legacy allowlist: a saved snapshot
// may carry a bounded legacy harness name that is retained as inert metadata.
func (r Receipt) ValidateMetadata() error {
	if err := r.Key.Validate(); err != nil {
		return err
	}
	if err := r.validateForm(); err != nil {
		return err
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

func (r Receipt) validateForm() error {
	switch {
	case r.Legacy():
		if r.Host != "" || r.Command != "" {
			return errors.New("receipt mixes a legacy harness with host and command")
		}
		if len(r.Harness) > 128 || !modelPattern.MatchString(r.Harness) {
			return errors.New("invalid receipt harness token")
		}
	case r.Host == "" && r.Command == "":
		return errors.New("receipt requires a host and command")
	case !ValidHost(r.Host):
		return errors.New("receipt host must be codex or claude")
	case !ValidCommand(r.Command):
		return errors.New("receipt command must be a nonreserved command name of at most 64 bytes")
	}
	return nil
}

// ClearGuard narrows a clear to the receipt whose fields match every supplied
// guard. An empty field matches any receipt under the key.
type ClearGuard struct {
	Command      string
	Conversation string
}

// Validate rejects a supplied command outside the name grammar and a supplied
// conversation that is not a UUID. Empty fields are wildcards and always pass.
func (g ClearGuard) Validate() error {
	if g.Command != "" && !ValidCommand(g.Command) {
		return errors.New("receipt command guard must be a bare command name")
	}
	if g.Conversation != "" && !ValidConversationID(g.Conversation) {
		return errors.New("receipt conversation must be a UUID")
	}
	return nil
}

// Matches reports whether r satisfies every supplied guard field. The command
// guard compares against the name rexurrect would type, so a launcher can
// clear a legacy receipt registered under the same command name. Conversation
// UUIDs compare case-insensitively because ValidConversationID accepts both
// hex cases.
func (g ClearGuard) Matches(r Receipt) bool {
	return (g.Command == "" || r.Name() == g.Command) &&
		(g.Conversation == "" || strings.EqualFold(r.Conversation, g.Conversation))
}

// InitialInput is constructed only from validated tokens and fixed syntax.
// An empty result means no bytes may be sent. Legacy receipts keep the exact
// decisions store format 1 made: claude-team never resumes.
func (r Receipt) InitialInput() string {
	if r.Validate() != nil {
		return ""
	}
	host := r.Host
	if r.Legacy() {
		switch r.Harness {
		case "claude", "claude-ddb":
			host = "claude"
		case "codex", "codex-team":
			host = "codex"
		default:
			return ""
		}
	}
	command := r.Name()
	switch host {
	case "claude":
		command += " --resume " + r.Conversation
		if r.Model != "" {
			command += " --model " + r.Model
		}
		if r.Effort != "" {
			command += " --effort " + r.Effort
		}
	case "codex":
		command += " resume " + r.Conversation
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
