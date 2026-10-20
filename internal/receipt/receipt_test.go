package receipt

import (
	"strings"
	"testing"
)

func validReceipt() Receipt {
	return Receipt{Key: Key{SessionID: "session:01234567-89ab-4cde-8fab-0123456789ab", BlockID: "block:01234567-89ab-4cde-8fab-0123456789ab"}, Harness: "claude", Conversation: "01234567-89ab-4cde-8fab-0123456789ab", Model: "model-1.2", Effort: "high"}
}

func TestInitialInput(t *testing.T) {
	for _, harness := range []string{"claude", "claude-ddb", "codex", "codex-team", "claude-team", "unknown"} {
		r := validReceipt()
		r.Harness = harness
		for _, optional := range []bool{false, true} {
			if !optional {
				r.Model, r.Effort = "", ""
			} else {
				r.Model, r.Effort = "model-1.2", "high"
			}
			var want string
			switch harness {
			case "claude", "claude-ddb":
				want = harness + " --resume " + r.Conversation
				if optional {
					want += " --model model-1.2 --effort high"
				}
			case "codex", "codex-team":
				want = harness + " resume " + r.Conversation
				if optional {
					want += ` -m model-1.2 -c model_reasoning_effort="high"`
				}
			}
			if want != "" {
				want += "\n"
			}
			if got := r.InitialInput(); got != want {
				t.Errorf("%s optional=%t: %q, want %q", harness, optional, got, want)
			}
		}
	}
}

func TestRejectInvalidReceipts(t *testing.T) {
	valid := validReceipt()
	for _, effort := range []string{"", "low", "medium", "high", "xhigh"} {
		r := valid
		r.Effort = effort
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"", "bad", "../claude", "CLAUDE", "claude;id", "$(id)", "`id`", "a b", "a\nb", "a\r", "a\x00b", "'", `"`, "a/b", "a\\b", strings.Repeat("a", 129)} {
		for _, field := range []string{"session", "block", "harness", "conversation", "model", "effort"} {
			if value == "" && (field == "model" || field == "effort") {
				continue
			}
			r := valid
			switch field {
			case "session":
				r.SessionID = value
			case "block":
				r.BlockID = value
			case "harness":
				r.Harness = value
			case "conversation":
				r.Conversation = value
			case "model":
				r.Model = value
			case "effort":
				r.Effort = value
			}
			// "bad" is a valid model token.
			if field == "model" && (value == "bad") {
				continue
			}
			if r.Validate() == nil || r.InitialInput() != "" {
				t.Errorf("accepted %s=%q", field, value)
			}
		}
	}
}

func TestClearGuard(t *testing.T) {
	r := Receipt{Harness: "codex", Conversation: "01234567-89ab-4cde-8fab-0123456789ab"}
	for _, tc := range []struct {
		guard ClearGuard
		match bool
	}{
		{ClearGuard{}, true},
		{ClearGuard{Harness: "codex"}, true},
		{ClearGuard{Harness: "codex-team"}, false},
		{ClearGuard{Conversation: r.Conversation}, true},
		{ClearGuard{Conversation: "00000000-0000-0000-0000-000000000000"}, false},
		{ClearGuard{Harness: "codex", Conversation: r.Conversation}, true},
		{ClearGuard{Harness: "codex", Conversation: "00000000-0000-0000-0000-000000000000"}, false},
		{ClearGuard{Harness: "codex-team", Conversation: r.Conversation}, false},
	} {
		if err := tc.guard.Validate(); err != nil {
			t.Fatalf("guard %+v: %v", tc.guard, err)
		}
		if got := tc.guard.Matches(r); got != tc.match {
			t.Errorf("guard %+v matches=%v, want %v", tc.guard, got, tc.match)
		}
	}
	for _, bad := range []ClearGuard{{Harness: "zsh"}, {Harness: "codex;id"}, {Harness: "Codex"}, {Conversation: "not-a-uuid"}, {Harness: "codex", Conversation: "thr_123"}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted guard %+v", bad)
		}
	}
}
