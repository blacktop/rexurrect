package snapshot

import (
	"strings"
	"testing"
)

func TestValidForeground(t *testing.T) {
	for _, ok := range []string{"vim", "slack-tooy chat --channel general", "echo 'héllo wörld'", strings.Repeat("x", MaxForeground)} {
		if err := ValidForeground(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"empty":     "",
		"oversized": strings.Repeat("x", MaxForeground+1),
		"multiline": "cat <<EOF\nhello",
		"delete":    "ls\x7f",
		"c1":        "ls\u009b",
		"bidi":      "ls\u202e",
		"leading":   " ls",
		"invalid":   "ls \xff",
	} {
		if err := ValidForeground(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestForegroundRequiresVersion5(t *testing.T) {
	s := scrollbackFixture(nil)
	s.Sessions[0].Windows[0].Layout.Block.Foreground = "vim"
	if _, err := s.Inventory(); err != nil {
		t.Fatal(err)
	}
	s.Version = 4
	if _, err := s.Inventory(); err == nil || !strings.Contains(err.Error(), "version 5") {
		t.Fatalf("v4 accepted a foreground command: %v", err)
	}
	s.Version = Version
	s.Sessions[0].Windows[0].Layout.Block.Foreground = "ls \x1b[2J"
	if _, err := s.Inventory(); err == nil || !strings.Contains(err.Error(), "non-printable") {
		t.Fatalf("control character accepted: %v", err)
	}
}
