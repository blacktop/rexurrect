package snapshot

import (
	"testing"

	"github.com/blacktop/rexurrect/internal/receipt"
)

// Each block falls in exactly one class, and a receipt decides the class
// even when a foreground command was recorded.
func TestRecoveryCounts(t *testing.T) {
	block := func(n int) SourceID {
		return SourceID("block:0000000" + string(rune('0'+n)) + "-89ab-4cde-8fab-0123456789ab")
	}
	key := func(n int) receipt.Key {
		return receipt.Key{SessionID: "session:01234567-89ab-4cde-8fab-0123456789ab", BlockID: string(block(n))}
	}
	live := receipt.Receipt{Key: key(1), Host: "claude", Command: "claude", Conversation: "01234567-89ab-4cde-8fab-0123456789ab"}
	inert := receipt.Receipt{Key: key(2), Harness: "claude-team", Conversation: "01234567-89ab-4cde-8fab-0123456789ab"}
	both := live
	both.Key = key(3)
	blocks := []Block{
		{ID: block(1), CWD: "/", AgentResume: &live},
		{ID: block(2), CWD: "/", AgentResume: &inert, Foreground: "vim"},
		{ID: block(3), CWD: "/", AgentResume: &both, Foreground: "vim"},
		{ID: block(4), CWD: "/", Foreground: "slack-tooy chat"},
		{ID: block(5), CWD: "/"},
	}
	s := Snapshot{Version: Version}
	for i := range blocks {
		b := blocks[i]
		s.Sessions = append(s.Sessions, Session{ID: SourceID("session:0000000" + string(rune('0'+i)) + "-89ab-4cde-8fab-0123456789ab"), ActiveWindow: "window",
			Windows: []Window{{ID: "window", LayerID: "layer", Focus: b.ID, Layout: Node{Block: &b}}}})
	}
	if got, want := s.Recovery(), (Recovery{Agents: 2, Typed: 1, Plain: 1, Inert: 1}); got != want {
		t.Fatalf("recovery = %+v, want %+v", got, want)
	}
	if got := (Snapshot{Version: Version}).Recovery(); got != (Recovery{}) {
		t.Fatalf("empty snapshot = %+v", got)
	}
}
