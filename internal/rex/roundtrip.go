package rex

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

const shellFlavor = "com.superlogical.terminal.shell"

const cwdAttempts = 3

type sessionView struct {
	ID      snapshot.LiveID `json:"session_id"`
	Label   string          `json:"label"`
	Active  snapshot.LiveID `json:"active_window_id"`
	Windows []windowView    `json:"windows"`
}
type windowView struct {
	ID     snapshot.LiveID `json:"window_id"`
	Label  string          `json:"label"`
	Focus  snapshot.LiveID `json:"focused_block_id"`
	Zoom   snapshot.LiveID `json:"zoomed_block_id"`
	Layers []layerView     `json:"layers"`
}
type layerView struct {
	ID     snapshot.LiveID `json:"layer_id"`
	Kind   string          `json:"kind"`
	Layout viewNode        `json:"layout"`
	Blocks []blockView     `json:"blocks"`
}
type blockView struct {
	ID     snapshot.LiveID `json:"block_id"`
	Label  string          `json:"label"`
	Flavor string          `json:"flavor"`
}
type listedBlock struct {
	blockView
	Placement string          `json:"placement"`
	WindowID  snapshot.LiveID `json:"window_id"`
}
type viewNode struct {
	BlockID snapshot.LiveID `json:"block_id"`
	Split   *viewSplit      `json:"split"`
}
type viewSplit struct {
	ID        snapshot.LiveID `json:"split_id"`
	Direction string          `json:"direction"`
	Ratio     float64         `json:"ratio"`
	Before    viewNode        `json:"before"`
	After     viewNode        `json:"after"`
}

func (c Client) List(ctx context.Context) ([]snapshot.LiveID, error) {
	var result struct {
		Sessions []struct {
			ID snapshot.LiveID `json:"session_id"`
		} `json:"sessions"`
	}
	if err := c.Call(ctx, "session.list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	if len(result.Sessions) > 64 {
		return nil, errors.New("too many sessions")
	}
	ids := make([]snapshot.LiveID, 0, len(result.Sessions))
	for _, s := range result.Sessions {
		ids = append(ids, s.ID)
	}
	return ids, nil
}

func (c Client) view(ctx context.Context, id snapshot.LiveID) (sessionView, error) {
	var result sessionView
	err := c.Call(ctx, "session.view", map[string]any{"session_id": id}, &result)
	if err == nil && result.ID != id {
		err = errors.New("session.view returned a different session")
	}
	return result, err
}

func (c Client) Capture(ctx context.Context, ids []snapshot.LiveID) (snapshot.Snapshot, snapshot.Report, error) {
	out := snapshot.Snapshot{Version: snapshot.Version}
	report := snapshot.Report{}
	if len(ids) > 64 {
		return out, report, errors.New("too many selected sessions")
	}
	budget, blockBudget := 16384, 16384
	for _, id := range ids {
		view, err := c.view(ctx, id)
		if err != nil {
			return out, report, err
		}
		var listed struct {
			Blocks []listedBlock `json:"blocks"`
		}
		if err := c.Call(ctx, "session.list_blocks", map[string]any{"session_id": id}, &listed); err != nil {
			return out, report, err
		}
		if len(listed.Blocks) > blockBudget {
			return out, report, errors.New("too many registered blocks")
		}
		blockBudget -= len(listed.Blocks)
		registered := make(map[snapshot.LiveID]listedBlock, len(listed.Blocks))
		for _, b := range listed.Blocks {
			if b.ID == "" || len(b.ID) > 256 || b.Flavor == "" || len(b.Flavor) > 4096 {
				return out, report, errors.New("invalid registered block")
			}
			if _, ok := registered[b.ID]; ok {
				return out, report, errors.New("duplicate registered block")
			}
			if b.Placement != "placed" && b.Placement != "detached" || (b.Placement == "detached") != (b.WindowID == "") {
				return out, report, errors.New("invalid registered block placement")
			}
			registered[b.ID] = b
		}
		s := snapshot.Session{ID: snapshot.SourceID(view.ID), Label: view.Label, ActiveWindow: snapshot.SourceID(view.Active)}
		if len(view.Windows) > 64 {
			return out, report, errors.New("too many windows")
		}
		for _, w := range view.Windows {
			window := snapshot.Window{ID: snapshot.SourceID(w.ID), Label: w.Label, Focus: snapshot.SourceID(w.Focus), Zoom: snapshot.SourceID(w.Zoom)}
			for _, layer := range w.Layers {
				for _, b := range layer.Blocks {
					listed, ok := registered[b.ID]
					if !ok || listed.Placement != "placed" || listed.WindowID != w.ID || listed.Flavor != b.Flavor {
						return out, report, fmt.Errorf("block %s differs between session.view and session.list_blocks", b.ID)
					}
					delete(registered, b.ID)
				}
				if layer.Kind != "tiled" {
					u := snapshot.Unsupported{SessionID: s.ID, WindowID: window.ID, LayerID: snapshot.SourceID(layer.ID), Kind: layer.Kind, Reason: "only the tiled base layer is supported"}
					for _, b := range layer.Blocks {
						u.BlockIDs = append(u.BlockIDs, snapshot.SourceID(b.ID))
					}
					report.Unsupported = append(report.Unsupported, u)
					continue
				}
				if window.LayerID != "" {
					return out, report, errors.New("multiple tiled layers")
				}
				window.LayerID = snapshot.SourceID(layer.ID)
				blocks := make(map[snapshot.LiveID]blockView, len(layer.Blocks))
				for _, b := range layer.Blocks {
					if _, ok := blocks[b.ID]; ok {
						return out, report, errors.New("duplicate block inventory")
					}
					blocks[b.ID] = b
				}
				window.Layout, err = c.captureNode(ctx, id, layer.Layout, blocks, 0, &budget, &report)
				if err != nil {
					return out, report, err
				}
				if len(blocks) != 0 {
					return out, report, errors.New("unplaced block in tiled inventory")
				}
			}
			s.Windows = append(s.Windows, window)
		}
		for _, b := range listed.Blocks {
			if _, ok := registered[b.ID]; !ok {
				continue
			}
			if b.Placement != "detached" {
				return out, report, fmt.Errorf("placed block %s missing from session.view", b.ID)
			}
			report.Unsupported = append(report.Unsupported, snapshot.Unsupported{SessionID: s.ID, Kind: "detached", BlockIDs: []snapshot.SourceID{snapshot.SourceID(b.ID)}, Flavor: b.Flavor, Reason: "detached block is absent from window layouts"})
		}
		out.Sessions = append(out.Sessions, s)
	}
	_, err := out.Inventory()
	return out, report, err
}

func (c Client) captureNode(ctx context.Context, session snapshot.LiveID, n viewNode, blocks map[snapshot.LiveID]blockView, depth int, budget *int, report *snapshot.Report) (snapshot.Node, error) {
	if depth > 64 || *budget <= 0 || (n.BlockID == "") == (n.Split == nil) {
		return snapshot.Node{}, errors.New("invalid or excessive Rex layout")
	}
	*budget--
	if n.Split != nil {
		p := n.Split
		before, err := c.captureNode(ctx, session, p.Before, blocks, depth+1, budget, report)
		if err != nil {
			return snapshot.Node{}, err
		}
		after, err := c.captureNode(ctx, session, p.After, blocks, depth+1, budget, report)
		if err != nil {
			return snapshot.Node{}, err
		}
		return snapshot.Node{Split: &snapshot.Split{ID: snapshot.SourceID(p.ID), Direction: p.Direction, Ratio: p.Ratio, Before: before, After: after}}, nil
	}
	b, ok := blocks[n.BlockID]
	if !ok || b.Flavor != shellFlavor {
		return snapshot.Node{}, fmt.Errorf("missing or unsupported shell block %s", n.BlockID)
	}
	delete(blocks, n.BlockID)
	cwd, err := c.captureCWD(ctx, session, n.BlockID, report)
	if err != nil {
		return snapshot.Node{}, err
	}
	return snapshot.Node{Block: &snapshot.Block{ID: snapshot.SourceID(b.ID), Label: b.Label, CWD: cwd}}, nil
}

func (c Client) captureCWD(ctx context.Context, session, block snapshot.LiveID, report *snapshot.Report) (string, error) {
	for attempt := range cwdAttempts {
		if attempt != 0 {
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
		cwd, err := c.terminalCWD(ctx, session, block)
		if err == nil {
			return cwd, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !errors.Is(err, errNoCWD) {
			return "", err
		}
	}
	home, err := snapshot.HomeCWD()
	if err != nil {
		return "", fmt.Errorf("block %s home cwd: %w", block, err)
	}
	report.CWDSubstitutions = append(report.CWDSubstitutions, snapshot.CWDSubstitution{BlockID: snapshot.SourceID(block), CWD: home})
	return home, nil
}

var errNoCWD = errors.New("no absolute existing cwd")

// terminalCWD decodes only cwd-bearing fields. Process command lines,
// environment, terminal contents, and OSC 7 ownership metadata are not saved.
func (c Client) terminalCWD(ctx context.Context, session, block snapshot.LiveID) (string, error) {
	args := map[string]any{"session_id": session, "block_id": block, "args": map[string]any{}}
	var pwd struct {
		PWD string `json:"pwd"`
	}
	pwdErr := c.Call(ctx, "com.superlogical.terminal.pwd", args, &pwd)
	if pwdErr == nil {
		var u *url.URL
		u, pwdErr = url.Parse(pwd.PWD)
		if pwdErr == nil {
			if u.Scheme != "file" {
				pwdErr = errors.New("pwd is not a file URL")
			} else {
				var cwd string
				cwd, pwdErr = snapshot.CanonicalCWD(u.Path)
				if pwdErr == nil {
					return cwd, nil
				}
			}
		}
	}
	var process struct {
		Foreground struct {
			CWD string `json:"cwd"`
		} `json:"foreground"`
		Child struct {
			CWD string `json:"cwd"`
		} `json:"child"`
	}
	processErr := c.Call(ctx, "com.superlogical.terminal.process", args, &process)
	if processErr != nil {
		return "", fmt.Errorf("block %s cwd read: %w", block, errors.Join(pwdErr, processErr))
	}
	cwd, processErr := snapshot.CanonicalCWD(process.Foreground.CWD)
	if processErr == nil {
		return cwd, nil
	}
	cwd, childErr := snapshot.CanonicalCWD(process.Child.CWD)
	if childErr == nil {
		return cwd, nil
	}
	processErr = errors.Join(fmt.Errorf("foreground: %w", processErr), fmt.Errorf("child: %w", childErr))
	return "", fmt.Errorf("block %s has %w: %w", block, errNoCWD, errors.Join(fmt.Errorf("pwd: %w", pwdErr), processErr))
}

type createWindow struct {
	Label  string     `json:"window_label"`
	Layout createNode `json:"layout"`
}
type createNode struct {
	Block *createBlock `json:"block,omitempty"`
	Split *createSplit `json:"split,omitempty"`
}
type createBlock struct {
	Flavor  string       `json:"flavor"`
	Label   string       `json:"label"`
	Options shellOptions `json:"options"`
}
type shellOptions struct {
	Shell        string   `json:"shell,omitempty"`
	Login        *bool    `json:"login,omitempty"`
	Command      []string `json:"command,omitempty"`
	CWD          string   `json:"cwd"`
	InitialInput string   `json:"initial_input,omitempty"`
}
type createSplit struct {
	Direction string     `json:"direction"`
	Ratio     float64    `json:"ratio"`
	Before    createNode `json:"before"`
	After     createNode `json:"after"`
}
type createdWindow struct {
	ID     snapshot.LiveID   `json:"window_id"`
	Blocks []snapshot.LiveID `json:"block_ids"`
}
type createdSession struct {
	ID      snapshot.LiveID `json:"session_id"`
	Windows []createdWindow `json:"initial_windows"`
}

// startCommands decides how a restored block starts: its scrollback replay
// command when it has one, else the clearing typed start for any block with
// initial input, else Rex's default shell chain. Every start command ends by
// starting that same chain, and initial_input is unchanged.
type startCommands struct {
	replay map[snapshot.SourceID]replayCommand
	typed  replayCommand
}

func (s startCommands) command(id snapshot.SourceID, input string) (replayCommand, bool) {
	if command, ok := s.replay[id]; ok {
		return command, true
	}
	if input != "" && s.typed != nil {
		return s.typed, true
	}
	return nil, false
}

// layout builds the create request. A receipt's resume command runs; a
// recorded foreground command is typed at the prompt, never run, so no
// newline follows it; a receipt takes precedence.
func layout(n snapshot.Node, leaves *[]snapshot.SourceID, starts startCommands) createNode {
	if n.Block != nil {
		b := n.Block
		*leaves = append(*leaves, b.ID)
		options := shellOptions{CWD: b.CWD}
		if b.AgentResume != nil {
			options.InitialInput = b.AgentResume.InitialInput()
		} else if b.Foreground != "" {
			options.InitialInput = b.Foreground
		}
		if command, ok := starts.command(b.ID, options.InitialInput); ok {
			login := false
			options.Shell, options.Login, options.Command = "none", &login, command
		}
		return createNode{Block: &createBlock{Flavor: shellFlavor, Label: b.Label, Options: options}}
	}
	p := n.Split
	return createNode{Split: &createSplit{Direction: p.Direction, Ratio: p.Ratio, Before: layout(p.Before, leaves, starts), After: layout(p.After, leaves, starts)}}
}

// Restore starts new shells. On error the returned map identifies every session
// whose creation was acknowledged, so the caller can inspect or clean up partial effects.
func (c Client) Restore(ctx context.Context, s snapshot.Snapshot) (snapshot.IDMap, error) {
	ids := make(snapshot.IDMap)
	if _, err := s.Inventory(); err != nil {
		return ids, err
	}
	for _, session := range s.Sessions {
		created, err := c.createSession(ctx, session, session.Label, startCommands{})
		if err != nil {
			return ids, err
		}
		if err := mapCreated(session, created, ids); err != nil {
			return ids, err
		}
		view, err := c.view(ctx, created.ID)
		if err != nil {
			return ids, err
		}
		if err := mapView(session, view, ids); err != nil {
			return ids, err
		}
		if err := c.configureSession(ctx, session, ids); err != nil {
			return ids, err
		}
	}
	return ids, nil
}

func (c Client) createSession(ctx context.Context, session snapshot.Session, label string, starts startCommands) (createdSession, error) {
	windows := make([]createWindow, 0, len(session.Windows))
	for _, w := range session.Windows {
		var leaves []snapshot.SourceID
		windows = append(windows, createWindow{Label: w.Label, Layout: layout(w.Layout, &leaves, starts)})
	}
	var created createdSession
	err := c.Call(ctx, "session.create", map[string]any{"label": label, "initial_windows": windows}, &created)
	return created, err
}

func putMapping(ids snapshot.IDMap, old snapshot.SourceID, live snapshot.LiveID) error {
	if live == "" || len(live) > 256 || ids[old] != "" && ids[old] != live {
		return fmt.Errorf("invalid or changed mapping for %s", old)
	}
	ids[old] = live
	return nil
}

func mapCreated(session snapshot.Session, created createdSession, ids snapshot.IDMap) error {
	if err := putMapping(ids, session.ID, created.ID); err != nil {
		return err
	}
	var err error
	if len(created.Windows) != len(session.Windows) {
		err = errors.New("session.create window inventory differs")
	}
	for i := 0; i < min(len(created.Windows), len(session.Windows)); i++ {
		w := created.Windows[i]
		err = errors.Join(err, putMapping(ids, session.Windows[i].ID, w.ID))
		var leaves []snapshot.SourceID
		layout(session.Windows[i].Layout, &leaves, startCommands{})
		if len(w.Blocks) != len(leaves) {
			err = errors.Join(err, errors.New("session.create block inventory differs"))
		}
		for j := 0; j < min(len(w.Blocks), len(leaves)); j++ {
			err = errors.Join(err, putMapping(ids, leaves[j], w.Blocks[j]))
		}
	}
	return err
}

func mapView(session snapshot.Session, view sessionView, ids snapshot.IDMap) error {
	if err := putMapping(ids, session.ID, view.ID); err != nil {
		return err
	}
	if len(view.Windows) != len(session.Windows) {
		return errors.New("created view window inventory differs")
	}
	for i, w := range session.Windows {
		live := view.Windows[i]
		if len(live.Layers) != 1 || live.Layers[0].Kind != "tiled" {
			return errors.New("created base layer differs")
		}
		if err := errors.Join(putMapping(ids, w.ID, live.ID), putMapping(ids, w.LayerID, live.Layers[0].ID)); err != nil {
			return err
		}
		if err := mapSplits(w.Layout, live.Layers[0].Layout, ids); err != nil {
			return err
		}
	}
	return nil
}

func (c Client) configureSession(ctx context.Context, session snapshot.Session, ids snapshot.IDMap) error {
	for _, w := range session.Windows {
		if err := c.Call(ctx, "session.focus_block", map[string]any{"session_id": ids[session.ID], "block_id": ids[w.Focus]}, nil); err != nil {
			return err
		}
		if err := c.Call(ctx, "session.zoom_block", map[string]any{"session_id": ids[session.ID], "block_id": ids[w.Focus], "zoom": w.Zoom != ""}, nil); err != nil {
			return err
		}
	}
	if session.ActiveWindow != "" {
		return c.Call(ctx, "session.focus_window", map[string]any{"session_id": ids[session.ID], "window_id": ids[session.ActiveWindow]}, nil)
	}
	return nil
}

func mapSplits(old snapshot.Node, live viewNode, ids snapshot.IDMap) error {
	if old.Block != nil {
		if live.Split != nil {
			return errors.New("created block placement differs")
		}
		return putMapping(ids, old.Block.ID, live.BlockID)
	}
	if live.Split == nil || live.BlockID != "" {
		return errors.New("created split placement differs")
	}
	if err := putMapping(ids, old.Split.ID, live.Split.ID); err != nil {
		return err
	}
	if err := mapSplits(old.Split.Before, live.Split.Before, ids); err != nil {
		return err
	}
	return mapSplits(old.Split.After, live.Split.After, ids)
}
