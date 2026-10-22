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
		"newline":   "ls\n",
		"tab":       "ls\tla",
		"escape":    "ls \x1b[2J",
		"delete":    "ls\x7f",
		"leading":   " ls",
		"trailing":  "ls ",
		"invalid":   "ls \xff",
	} {
		if err := ValidForeground(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestForegroundRequiresVersion5(t *testing.T) {
	block := SourceID("block:01234567-89ab-4cde-8fab-0123456789ab")
	s := Snapshot{Version: Version, Sessions: []Session{{ID: "session:01234567-89ab-4cde-8fab-0123456789ab", ActiveWindow: "window",
		Windows: []Window{{ID: "window", LayerID: "layer", Focus: block, Layout: Node{Block: &Block{ID: block, CWD: "/", Foreground: "vim"}}}}}}}
	if _, err := s.Inventory(); err != nil {
		t.Fatal(err)
	}
	s.Version = 4
	if _, err := s.Inventory(); err == nil || !strings.Contains(err.Error(), "version 5") {
		t.Fatalf("v4 accepted a foreground command: %v", err)
	}
	s.Version = Version
	s.Sessions[0].Windows[0].Layout.Block.Foreground = "ls \x1b[2J"
	if _, err := s.Inventory(); err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("control character accepted: %v", err)
	}
}
