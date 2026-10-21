package snapshot

import (
	"strings"
	"testing"
)

func scrollbackFixture(ref *ScrollbackRef) Snapshot {
	block := SourceID("block:01234567-89ab-4cde-8fab-0123456789ab")
	return Snapshot{Version: Version, Sessions: []Session{{ID: "session:01234567-89ab-4cde-8fab-0123456789ab", ActiveWindow: "window",
		Windows: []Window{{ID: "window", LayerID: "layer", Focus: block,
			Layout: Node{Block: &Block{ID: block, CWD: "/", Scrollback: ref}}}}}}}
}

func TestScrollbackReferenceValidation(t *testing.T) {
	valid := &ScrollbackRef{KeyID: strings.Repeat("0123", 8), ObjectID: strings.Repeat("89ab", 16)}
	s := scrollbackFixture(valid)
	if _, err := s.Inventory(); err != nil {
		t.Fatal(err)
	}
	for version := 1; version < 4; version++ {
		s.Version = version
		if _, err := s.Inventory(); err == nil || !strings.Contains(err.Error(), "snapshot version 4") {
			t.Fatalf("version %d accepted a scrollback reference: %v", version, err)
		}
	}
	s.Version = 3
	s.Sessions[0].Windows[0].Layout.Block.Scrollback = nil
	if _, err := s.Inventory(); err != nil {
		t.Fatalf("version 3 without a reference: %v", err)
	}
	for name, ref := range map[string]*ScrollbackRef{
		"empty":        {},
		"short key":    {KeyID: "0123", ObjectID: valid.ObjectID},
		"short object": {KeyID: valid.KeyID, ObjectID: "89ab"},
		"not hex":      {KeyID: strings.Repeat("zz", 16), ObjectID: valid.ObjectID},
		"path":         {KeyID: valid.KeyID, ObjectID: "../" + valid.ObjectID[3:]},
	} {
		s := scrollbackFixture(ref)
		if _, err := s.Inventory(); err == nil {
			t.Errorf("%s reference accepted", name)
		}
	}
}

func TestScrollbackReferenceIsNotTopology(t *testing.T) {
	with := scrollbackFixture(&ScrollbackRef{KeyID: strings.Repeat("0123", 8), ObjectID: strings.Repeat("89ab", 16)})
	without := scrollbackFixture(nil)
	ids := IDMap{}
	for _, id := range []SourceID{"session:01234567-89ab-4cde-8fab-0123456789ab", "window", "layer", "block:01234567-89ab-4cde-8fab-0123456789ab"} {
		ids[id] = LiveID(id)
	}
	if err := Compare(with, without, ids); err != nil {
		t.Fatalf("scrollback reference changed the observable topology: %v", err)
	}
	if err := Compare(without, with, ids); err != nil {
		t.Fatalf("scrollback reference changed the observable topology: %v", err)
	}
}
