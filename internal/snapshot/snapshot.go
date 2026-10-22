// Package snapshot defines the supported, versioned Rex resurrection contract.
package snapshot

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"errors"
	"github.com/blacktop/rexurrect/internal/receipt"
	"github.com/blacktop/rexurrect/internal/scrollback"
	"strings"
	"unicode/utf8"
)

// Version 2 added agent receipts; version 3 added their host and command form;
// version 4 added encrypted scrollback references; version 5 added recorded
// foreground commands.
const Version = 5

// MaxForeground bounds a recorded foreground command line in bytes.
const MaxForeground = 1024

type SourceID string
type LiveID string
type IDMap map[SourceID]LiveID

type Snapshot struct {
	Version  int       `json:"version"`
	Sessions []Session `json:"sessions"`
}

type Session struct {
	ID           SourceID `json:"source_id"`
	Label        string   `json:"label"`
	ActiveWindow SourceID `json:"active_window"`
	Windows      []Window `json:"windows"`
}

type Window struct {
	ID      SourceID `json:"source_id"`
	Label   string   `json:"label"`
	LayerID SourceID `json:"layer_source_id"`
	Layout  Node     `json:"layout"`
	Focus   SourceID `json:"focused_block"`
	Zoom    SourceID `json:"zoomed_block,omitempty"`
}

type Node struct {
	Block *Block `json:"block,omitempty"`
	Split *Split `json:"split,omitempty"`
}

type Block struct {
	ID          SourceID         `json:"source_id"`
	Label       string           `json:"label"`
	CWD         string           `json:"cwd"`
	AgentResume *receipt.Receipt `json:"agent_resume,omitempty"`
	Scrollback  *ScrollbackRef   `json:"scrollback,omitempty"`
	// Foreground is the command line the pane was running when it was saved,
	// as the user typed it. Restore types it at the new prompt and never runs
	// it; a receipt takes precedence.
	Foreground string `json:"foreground,omitempty"`
}

// ValidForeground accepts one trimmed line of printable UTF-8 of at most
// MaxForeground bytes. A control character could execute or corrupt the typed
// text, so none is allowed, including the newline that would run it.
func ValidForeground(command string) error {
	if command == "" || len(command) > MaxForeground {
		return fmt.Errorf("foreground command must be 1 to %d bytes", MaxForeground)
	}
	if !utf8.ValidString(command) {
		return errors.New("foreground command is not valid UTF-8")
	}
	if strings.TrimSpace(command) != command {
		return errors.New("foreground command has leading or trailing whitespace")
	}
	for _, r := range command {
		if r < 0x20 || r == 0x7f {
			return errors.New("foreground command contains a control character")
		}
	}
	return nil
}

// ScrollbackRef names the encrypted scrollback sidecar saved for a block: the
// key that sealed it and its keyed content identity. The sidecar itself lives
// outside the generation file.
type ScrollbackRef struct {
	KeyID    string `json:"key_id"`
	ObjectID string `json:"object_id"`
}

// Parse decodes both identifiers; Validate is the check alone.
func (r ScrollbackRef) Parse() (scrollback.KeyID, scrollback.ObjectID, error) {
	keyID, err := scrollback.ParseKeyID(r.KeyID)
	if err != nil {
		return keyID, scrollback.ObjectID{}, err
	}
	objectID, err := scrollback.ParseObjectID(r.ObjectID)
	return keyID, objectID, err
}

func (r ScrollbackRef) Validate() error {
	_, _, err := r.Parse()
	return err
}

type Split struct {
	ID        SourceID `json:"source_id"`
	Direction string   `json:"direction"`
	Ratio     float64  `json:"ratio"`
	Before    Node     `json:"before"`
	After     Node     `json:"after"`
}

type Unsupported struct {
	SessionID SourceID   `json:"session_source_id"`
	WindowID  SourceID   `json:"window_source_id"`
	LayerID   SourceID   `json:"layer_source_id"`
	Kind      string     `json:"kind"`
	BlockIDs  []SourceID `json:"block_source_ids"`
	Flavor    string     `json:"flavor,omitempty"`
	Reason    string     `json:"reason"`
}

type Report struct {
	Unsupported       []Unsupported     `json:"unsupported,omitempty"`
	PrunedReceipts    []SourceID        `json:"pruned_receipts,omitempty"`
	CWDSubstitutions  []CWDSubstitution `json:"cwd_substitutions,omitempty"`
	ScrollbackSkipped []Skip            `json:"scrollback_skipped,omitempty"`
	ForegroundSkipped []Skip            `json:"foreground_skipped,omitempty"`
}

// Skip records a shell block whose scrollback was deliberately not
// saved, and why; its layout, cwd, and receipt are unaffected.
type Skip struct {
	BlockID SourceID `json:"block_source_id"`
	Reason  string   `json:"reason"`
}

// CWDSubstitution records a capture that used home because the terminal had
// no observable working directory after bounded retries.
type CWDSubstitution struct {
	BlockID SourceID `json:"block_source_id"`
	CWD     string   `json:"cwd"`
}

// Blocks returns every tiled block in window and layout order. The pointers
// alias the session, so callers may annotate blocks in place. Call it only
// after Inventory has validated and bounded the layout trees.
func (s Session) Blocks() []*Block {
	var blocks []*Block
	var visit func(Node)
	visit = func(n Node) {
		if n.Block != nil {
			blocks = append(blocks, n.Block)
			return
		}
		visit(n.Split.Before)
		visit(n.Split.After)
	}
	for _, window := range s.Windows {
		visit(window.Layout)
	}
	return blocks
}

// Inventory validates structure and references, including IDs of split edges.
// Its resource bounds also make subsequent recursive walks safe.
func (s Snapshot) Inventory() (map[SourceID]string, error) {
	if s.Version < 1 || s.Version > Version || len(s.Sessions) > 64 {
		return nil, fmt.Errorf("unsupported snapshot version or session count")
	}
	ids := make(map[SourceID]string)
	add := func(id SourceID, kind string) error {
		if id == "" || len(id) > 256 || ids[id] != "" || len(ids) >= 16384 {
			return fmt.Errorf("invalid or duplicate %s ID %q", kind, id)
		}
		ids[id] = kind
		return nil
	}
	for _, session := range s.Sessions {
		if err := add(session.ID, "session"); err != nil {
			return nil, err
		}
		if len(session.Label) > 4096 || len(session.Windows) > 64 {
			return nil, fmt.Errorf("session %s exceeds bounds", session.ID)
		}
		windows := make(map[SourceID]bool)
		for _, window := range session.Windows {
			if err := add(window.ID, "window"); err != nil {
				return nil, err
			}
			if err := add(window.LayerID, "layer"); err != nil {
				return nil, err
			}
			if len(window.Label) > 4096 {
				return nil, fmt.Errorf("window label exceeds bound")
			}
			windows[window.ID] = true
			leaves := make(map[SourceID]bool)
			var walk func(Node, int) error
			walk = func(n Node, depth int) error {
				if depth > 64 || (n.Block == nil) == (n.Split == nil) {
					return fmt.Errorf("invalid layout in %s", window.ID)
				}
				if b := n.Block; b != nil {
					if r := b.AgentResume; r != nil {
						if s.Version < 2 {
							return fmt.Errorf("agent receipts require snapshot version 2")
						}
						if !r.Legacy() && s.Version < 3 {
							return fmt.Errorf("host and command receipts require snapshot version 3")
						}
						if err := r.ValidateMetadata(); err != nil || r.SessionID != string(session.ID) || r.BlockID != string(b.ID) {
							return fmt.Errorf("invalid agent receipt for block %s: %v", b.ID, err)
						}
					}
					if ref := b.Scrollback; ref != nil {
						if s.Version < 4 {
							return fmt.Errorf("scrollback references require snapshot version 4")
						}
						if err := ref.Validate(); err != nil {
							return fmt.Errorf("invalid scrollback reference for block %s: %w", b.ID, err)
						}
					}
					if b.Foreground != "" {
						if s.Version < 5 {
							return fmt.Errorf("foreground commands require snapshot version 5")
						}
						if err := ValidForeground(b.Foreground); err != nil {
							return fmt.Errorf("invalid foreground command for block %s: %w", b.ID, err)
						}
					}
					if !filepath.IsAbs(b.CWD) || len(b.CWD) > 4096 || len(b.Label) > 4096 {
						return fmt.Errorf("invalid block %s cwd or label", b.ID)
					}
					if err := add(b.ID, "block"); err != nil {
						return err
					}
					leaves[b.ID] = true
					return nil
				}
				p := n.Split
				if (p.Direction != "horizontal" && p.Direction != "vertical") || math.IsNaN(p.Ratio) || p.Ratio <= 0 || p.Ratio >= 1 {
					return fmt.Errorf("invalid split %s", p.ID)
				}
				if err := add(p.ID, "split"); err != nil {
					return err
				}
				if err := walk(p.Before, depth+1); err != nil {
					return err
				}
				return walk(p.After, depth+1)
			}
			if err := walk(window.Layout, 0); err != nil {
				return nil, err
			}
			if !leaves[window.Focus] || (window.Zoom != "" && (!leaves[window.Zoom] || window.Zoom != window.Focus)) {
				return nil, fmt.Errorf("invalid focus or zoom in %s", window.ID)
			}
		}
		if (len(windows) == 0 && session.ActiveWindow != "") || (len(windows) > 0 && !windows[session.ActiveWindow]) {
			return nil, fmt.Errorf("invalid active window in %s", session.ID)
		}
	}
	return ids, nil
}

// CanonicalCWD resolves symlinks rather than trusting a shell-reported PWD.
func CanonicalCWD(path string) (string, error) {
	if !filepath.IsAbs(path) || len(path) > 4096 {
		return "", fmt.Errorf("cwd is not absolute: %q", path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cwd %q: %w", path, syscall.ENOTDIR)
	}
	if len(canonical) > 4096 {
		return "", fmt.Errorf("cwd is not a valid directory: %q", path)
	}
	return canonical, nil
}

// HomeCWD is the canonical substitute for a terminal whose working directory
// is unobservable at capture or unusable at restore.
func HomeCWD() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return CanonicalCWD(home)
}

// Compare requires a total, one-to-one, kind-preserving map. It normalizes
// working directories and translates identities, never matching display labels.
func Compare(want, got Snapshot, ids IDMap) error {
	wanted, err := want.Inventory()
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	actual, err := got.Inventory()
	if err != nil {
		return fmt.Errorf("observed: %w", err)
	}
	if len(ids) != len(wanted) || len(actual) != len(wanted) {
		return fmt.Errorf("identity count differs: source=%d observed=%d map=%d", len(wanted), len(actual), len(ids))
	}
	used := make(map[LiveID]bool, len(ids))
	for id, kind := range wanted {
		live, ok := ids[id]
		if !ok || live == "" || used[live] || actual[SourceID(live)] != kind {
			return fmt.Errorf("missing, duplicate, or wrong-kind mapping for %s", id)
		}
		used[live] = true
	}
	a, err := normalize(want, func(id SourceID) SourceID { return SourceID(ids[id]) })
	if err != nil {
		return err
	}
	b, err := normalize(got, func(id SourceID) SourceID { return id })
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("mapped session topology, labels, cwd, focus, or zoom differ")
	}
	return nil
}

func normalize(s Snapshot, translate func(SourceID) SourceID) (Snapshot, error) {
	var node func(Node) (Node, error)
	node = func(n Node) (Node, error) {
		if n.Block != nil {
			b := *n.Block
			b.ID = translate(b.ID)
			// Receipts, scrollback, and foreground commands are replay
			// metadata, not observable Rex topology.
			b.AgentResume = nil
			b.Scrollback = nil
			b.Foreground = ""
			var err error
			b.CWD, err = CanonicalCWD(b.CWD)
			return Node{Block: &b}, err
		}
		p := *n.Split
		p.ID = translate(p.ID)
		var err error
		p.Before, err = node(p.Before)
		if err != nil {
			return Node{}, err
		}
		p.After, err = node(p.After)
		return Node{Split: &p}, err
	}
	out := Snapshot{Version: Version, Sessions: make([]Session, 0, len(s.Sessions))}
	for _, session := range s.Sessions {
		copy := Session{ID: translate(session.ID), Label: session.Label, ActiveWindow: translate(session.ActiveWindow), Windows: make([]Window, 0, len(session.Windows))}
		for _, w := range session.Windows {
			w.ID = translate(w.ID)
			w.LayerID = translate(w.LayerID)
			w.Focus = translate(w.Focus)
			w.Zoom = translate(w.Zoom)
			var err error
			w.Layout, err = node(w.Layout)
			if err != nil {
				return Snapshot{}, err
			}
			copy.Windows = append(copy.Windows, w)
		}
		out.Sessions = append(out.Sessions, copy)
	}
	return out, nil
}
