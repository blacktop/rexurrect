package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func fixture(prefix, root string) Snapshot {
	id := func(s string) SourceID { return SourceID(prefix + s) }
	out := Snapshot{Version: Version}
	for i := 1; i <= 2; i++ {
		base := fmt.Sprintf("s%d", i)
		leaf := func(name string) Node {
			return Node{Block: &Block{ID: id(base + "." + name), Label: name, CWD: filepath.Join(root, name)}}
		}
		nested := Node{Split: &Split{ID: id(base + ".x"), Direction: "horizontal", Ratio: 0.37, Before: leaf("left"), After: Node{Split: &Split{ID: id(base + ".y"), Direction: "vertical", Ratio: 0.61, Before: leaf("top"), After: leaf("bottom")}}}}
		out.Sessions = append(out.Sessions, Session{ID: id(base), Label: "duplicate label", ActiveWindow: id(base + ".w1"), Windows: []Window{
			{ID: id(base + ".w0"), Label: "first", LayerID: id(base + ".l0"), Layout: leaf("first"), Focus: id(base + ".first")},
			{ID: id(base + ".w1"), Label: "work", LayerID: id(base + ".l1"), Layout: nested, Focus: id(base + ".bottom"), Zoom: id(base + ".bottom")},
		}})
	}
	return out
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"first", "left", "top", "bottom", "other"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func fixtureMap(t *testing.T, s Snapshot) IDMap {
	t.Helper()
	inventory, err := s.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	ids := make(IDMap, len(inventory))
	for id := range inventory {
		ids[id] = LiveID("new:" + id)
	}
	return ids
}

func TestCompareMutations(t *testing.T) {
	root := fixtureRoot(t)
	want := fixture("", root)
	if err := Compare(want, fixture("new:", root), fixtureMap(t, want)); err != nil {
		t.Fatalf("valid comparison: %v", err)
	}
	tests := []struct {
		name   string
		change func(*Snapshot, IDMap)
	}{
		{"session omitted", func(s *Snapshot, _ IDMap) { s.Sessions = s.Sessions[:1] }},
		{"session altered", func(s *Snapshot, _ IDMap) { s.Sessions[0].Label = "changed" }},
		{"window omitted", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows = s.Sessions[0].Windows[1:] }},
		{"window order", func(s *Snapshot, _ IDMap) { w := s.Sessions[0].Windows; w[0], w[1] = w[1], w[0] }},
		{"window label", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows[1].Label = "changed" }},
		{"pane omitted", func(s *Snapshot, _ IDMap) { p := s.Sessions[0].Windows[1].Layout.Split; p.After = p.After.Split.After }},
		{"pane altered", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows[1].Layout.Split.Before.Block.Label = "changed" }},
		{"split edge omitted", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows[1].Layout.Split.After = Node{} }},
		{"split edges swapped", func(s *Snapshot, _ IDMap) {
			p := s.Sessions[0].Windows[1].Layout.Split
			p.Before, p.After = p.After, p.Before
		}},
		{"split direction", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows[1].Layout.Split.Direction = "vertical" }},
		{"split ratio", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows[1].Layout.Split.Ratio = 0.42 }},
		{"cwd omitted", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows[1].Layout.Split.Before.Block.CWD = "" }},
		{"cwd altered", func(s *Snapshot, _ IDMap) {
			s.Sessions[0].Windows[1].Layout.Split.Before.Block.CWD = filepath.Join(root, "other")
		}},
		{"active window omitted", func(s *Snapshot, _ IDMap) { s.Sessions[0].ActiveWindow = "" }},
		{"active window altered", func(s *Snapshot, _ IDMap) { s.Sessions[0].ActiveWindow = s.Sessions[0].Windows[0].ID }},
		{"focus omitted", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows[1].Focus = "" }},
		{"focus altered", func(s *Snapshot, _ IDMap) {
			w := &s.Sessions[0].Windows[1]
			w.Focus = w.Layout.Split.Before.Block.ID
			w.Zoom = w.Focus
		}},
		{"focus map altered", func(_ *Snapshot, m IDMap) { m["s1.bottom"], m["s1.top"] = m["s1.top"], m["s1.bottom"] }},
		{"zoom omitted", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows[1].Zoom = "" }},
		{"zoom altered", func(s *Snapshot, _ IDMap) { s.Sessions[0].Windows[1].Zoom = "new:s1.top" }},
		{"map altered", func(_ *Snapshot, m IDMap) { m["s1.x"] = "new:s1.left" }},
		{"map extra", func(_ *Snapshot, m IDMap) { m["extra"] = "new:extra" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := fixture("new:", root)
			ids := fixtureMap(t, want)
			test.change(&got, ids)
			if err := Compare(want, got, ids); err == nil {
				t.Fatal("mutation was accepted")
			}
		})
	}
	for id := range fixtureMap(t, want) {
		t.Run("missing mapping/"+string(id), func(t *testing.T) {
			ids := fixtureMap(t, want)
			delete(ids, id)
			if err := Compare(want, fixture("new:", root), ids); err == nil {
				t.Fatalf("missing %s accepted", id)
			}
		})
	}
}

func TestCompareCanonicalCWD(t *testing.T) {
	root := fixtureRoot(t)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "left"), alias); err != nil {
		t.Fatal(err)
	}
	want, got := fixture("", root), fixture("new:", root)
	got.Sessions[0].Windows[1].Layout.Split.Before.Block.CWD = alias
	if err := Compare(want, got, fixtureMap(t, want)); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryBounds(t *testing.T) {
	s := fixture("", fixtureRoot(t))
	s.Version++
	if _, err := s.Inventory(); err == nil {
		t.Fatal("unknown version accepted")
	}
	s.Version = Version
	p := s.Sessions[0].Windows[1].Layout.Split
	p.After = Node{Split: p}
	if _, err := s.Inventory(); err == nil {
		t.Fatal("cyclic layout accepted")
	}
}
