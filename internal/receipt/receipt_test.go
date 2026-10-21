package receipt

import (
	"strings"
	"testing"
)

const (
	testSession      = "session:01234567-89ab-4cde-8fab-0123456789ab"
	testBlock        = "block:01234567-89ab-4cde-8fab-0123456789ab"
	testConversation = "01234567-89ab-4cde-8fab-0123456789ab"
)

func commandReceipt(host, command string) Receipt {
	return Receipt{SessionID: testSession, BlockID: testBlock, Host: host, Command: command,
		Conversation: testConversation, Model: "model-1.2", Effort: "high"}
}

func legacyReceipt(harness string) Receipt {
	return Receipt{SessionID: testSession, BlockID: testBlock, Harness: harness,
		Conversation: testConversation, Model: "model-1.2", Effort: "high"}
}

func wantInput(host, name string, optional bool) string {
	var want string
	switch host {
	case "claude":
		want = name + " --resume " + testConversation
		if optional {
			want += " --model model-1.2 --effort high"
		}
	case "codex":
		want = name + " resume " + testConversation
		if optional {
			want += ` -m model-1.2 -c model_reasoning_effort="high"`
		}
	case "omp":
		want = name + " --resume " + testConversation
	default:
		return ""
	}
	return want + "\n"
}

func TestInitialInputByHost(t *testing.T) {
	for _, tc := range []struct{ host, command string }{
		{"claude", "claude"}, {"claude", "claude-work"}, {"claude", "my_claude.v2"},
		{"codex", "codex"}, {"codex", "codex-work"}, {"codex", "Codex_Team"},
		{"omp", "omp"}, {"omp", "omp-work"},
	} {
		for _, optional := range []bool{false, true} {
			if tc.host == "omp" && optional {
				continue // omp takes no model or effort
			}
			r := commandReceipt(tc.host, tc.command)
			if !optional {
				r.Model, r.Effort = "", ""
			}
			if got, want := r.InitialInput(), wantInput(tc.host, tc.command, optional); got != want {
				t.Errorf("%s %s optional=%t: %q, want %q", tc.host, tc.command, optional, got, want)
			}
		}
	}
}

func TestInitialInputOmp(t *testing.T) {
	valid := commandReceipt("omp", "omp")
	valid.Model, valid.Effort = "", ""
	valid.Conversation = "01a15c18-8aae-7774-8c25-1eb59ff7f355"
	if got, want := valid.InitialInput(), "omp --resume "+valid.Conversation+"\n"; got != want {
		t.Errorf("InitialInput(%+v) = %q, want %q", valid, got, want)
	}
	for _, mutate := range []func(*Receipt){func(r *Receipt) { r.Model = "m" }, func(r *Receipt) { r.Effort = "high" }} {
		r := valid
		mutate(&r)
		if r.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want error", r)
		}
	}
}

func TestInitialInputRejectsShellKeywords(t *testing.T) {
	for _, name := range []string{"if", "while", "function", "command", "exec", "_", "begin", "end", "and", "or", "not", "coproc", "foreach", "repeat", "nocorrect", "typeset"} {
		r := commandReceipt("codex", name)
		if r.Validate() == nil || r.InitialInput() != "" {
			t.Errorf("accepted shell keyword %q", name)
		}
	}
}

// Legacy receipts keep the exact decisions store format 1 made.
func TestLegacyInitialInput(t *testing.T) {
	for harness, host := range map[string]string{
		"claude": "claude", "claude-ddb": "claude", "codex": "codex", "codex-team": "codex",
		"claude-team": "", "unknown": "", "future-agent": "",
	} {
		r := legacyReceipt(harness)
		if err := r.ValidateMetadata(); err != nil {
			t.Fatalf("%s metadata: %v", harness, err)
		}
		if (r.Validate() == nil) != legacyHarness(harness) {
			t.Errorf("%s: allowlist decision changed: %v", harness, r.Validate())
		}
		if got, want := r.InitialInput(), wantInput(host, harness, true); got != want {
			t.Errorf("%s: %q, want %q", harness, got, want)
		}
		if r.Name() != harness || !r.Legacy() {
			t.Errorf("%s: name/legacy accessors", harness)
		}
	}
}

func TestRejectInvalidReceipts(t *testing.T) {
	valid := commandReceipt("codex", "codex")
	for _, effort := range []string{"", "low", "medium", "high", "xhigh"} {
		r := valid
		r.Effort = effort
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	values := []string{"", "bad", "../claude", "CLAUDE", "claude;id", "$(id)", "`id`", "a b", "a\nb",
		"a\r", "a\x00b", "'", `"`, "a/b", "a\\b", "-codex", "--resume", strings.Repeat("a", 129)}
	for _, value := range values {
		for _, field := range []string{"session", "block", "host", "command", "conversation", "model", "effort"} {
			if value == "" && (field == "model" || field == "effort") {
				continue
			}
			r := valid
			switch field {
			case "session":
				r.SessionID = value
			case "block":
				r.BlockID = value
			case "host":
				r.Host = value
			case "command":
				r.Command = value
			case "conversation":
				r.Conversation = value
			case "model":
				r.Model = value
			case "effort":
				r.Effort = value
			}
			// "bad" is a valid model token; command names may also be uppercase.
			if value == "bad" && field == "model" || (value == "bad" || value == "CLAUDE") && field == "command" {
				continue
			}
			if r.Validate() == nil || r.InitialInput() != "" {
				t.Errorf("accepted %s=%q", field, value)
			}
		}
	}
	if r := commandReceipt("codex", strings.Repeat("a", 64)); r.Validate() != nil {
		t.Error("rejected a 64-byte command name")
	}
	if r := commandReceipt("codex", strings.Repeat("a", 65)); r.Validate() == nil {
		t.Error("accepted a 65-byte command name")
	}
}

// Mixed or half-formed receipts are rejected in both validators, so neither a
// store nor a snapshot can smuggle one form's name into the other's syntax.
func TestRejectMixedForms(t *testing.T) {
	mixed := commandReceipt("codex", "codex")
	mixed.Harness = "codex"
	hostOnly := commandReceipt("codex", "")
	commandOnly := commandReceipt("", "codex")
	for name, r := range map[string]Receipt{"mixed": mixed, "host-only": hostOnly, "command-only": commandOnly, "empty": {SessionID: testSession, BlockID: testBlock, Conversation: testConversation}} {
		if r.ValidateMetadata() == nil || r.Validate() == nil || r.InitialInput() != "" {
			t.Errorf("accepted %s receipt", name)
		}
	}
	legacy := legacyReceipt("codex")
	legacy.Harness = strings.Repeat("a", 129)
	if legacy.ValidateMetadata() == nil {
		t.Error("accepted an oversized legacy harness")
	}
}

func TestClearGuard(t *testing.T) {
	r := commandReceipt("codex", "codex-work")
	legacy := legacyReceipt("codex")
	for _, tc := range []struct {
		guard ClearGuard
		match bool
	}{
		{ClearGuard{}, true},
		{ClearGuard{Command: "codex-work"}, true},
		{ClearGuard{Command: "codex"}, false},
		{ClearGuard{Conversation: r.Conversation}, true},
		{ClearGuard{Conversation: strings.ToUpper(r.Conversation)}, true},
		{ClearGuard{Conversation: "00000000-0000-0000-0000-000000000000"}, false},
		{ClearGuard{Command: "codex-work", Conversation: r.Conversation}, true},
		{ClearGuard{Command: "codex-work", Conversation: "00000000-0000-0000-0000-000000000000"}, false},
		{ClearGuard{Command: "codex", Conversation: r.Conversation}, false},
	} {
		if err := tc.guard.Validate(); err != nil {
			t.Fatalf("guard %+v: %v", tc.guard, err)
		}
		if got := tc.guard.Matches(r); got != tc.match {
			t.Errorf("guard %+v matches=%v, want %v", tc.guard, got, tc.match)
		}
	}
	// A launcher clears a legacy receipt registered under the same command name.
	if !(ClearGuard{Command: "codex"}).Matches(legacy) || (ClearGuard{Command: "codex-work"}).Matches(legacy) {
		t.Error("command guard does not match legacy receipts by name")
	}
	for _, bad := range []ClearGuard{{Command: "-codex"}, {Command: "codex;id"}, {Command: "a b"}, {Conversation: "not-a-uuid"}, {Command: "codex", Conversation: "thr_123"}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted guard %+v", bad)
		}
	}
}
