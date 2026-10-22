package rex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blacktop/rexurrect/internal/keychain/keychaintest"
	"github.com/blacktop/rexurrect/internal/scrollback"
	"github.com/blacktop/rexurrect/internal/snapshot"
	"github.com/blacktop/rexurrect/internal/state"
)

const scrollCanary = "SCROLL-CANARY"

func blockText(t *testing.T, c Client, session, block snapshot.LiveID) string {
	t.Helper()
	var out struct {
		Content string `json:"content"`
	}
	if err := c.Call(t.Context(), "com.superlogical.terminal.format", map[string]any{"session_id": session, "block_id": block, "args": map[string]any{"format": "text"}}, &out); err != nil {
		t.Fatal(err)
	}
	return out.Content
}

func waitBlockText(t *testing.T, c Client, session, block snapshot.LiveID, want string) string {
	t.Helper()
	deadline := time.Now().Add(DefaultAutoWait)
	var text string
	for time.Now().Before(deadline) {
		text = blockText(t, c, session, block)
		if strings.Contains(text, want) {
			return text
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("block %s never showed %q:\n%s", block, want, text)
	return ""
}

func waitScreen(t *testing.T, c Client, session, block snapshot.LiveID, want string) {
	t.Helper()
	deadline := time.Now().Add(DefaultAutoWait)
	for time.Now().Before(deadline) {
		var m terminalModes
		if err := c.Call(t.Context(), "com.superlogical.terminal.modes", map[string]any{"session_id": session, "block_id": block, "args": map[string]any{}}, &m); err == nil && m.Screen == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("block %s never reached the %s screen", block, want)
}

// No file under the state directory may hold the canary in clear.
func assertNoPlaintext(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(scrollCanary)) {
			t.Errorf("plaintext scrollback in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestScrollbackCaptureAndReplayPrivateServer(t *testing.T) {
	requireLoginShell(t)
	fake := keychaintest.New(t, ScrollbackKeychainService)
	s := newIsolatedServerWithSetup(t, func(s *isolatedServer) {
		s.client.Env = append(s.client.Env, "REXURRECT_SECURITY_TOOL="+fake.Security, "RR_ITEMS="+fake.Items)
	})
	cli := buildReceiptCLI(t, s.root)
	previous := rexurrectExecutable
	rexurrectExecutable = func() (string, error) { return cli, nil }
	t.Cleanup(func() { rexurrectExecutable = previous })
	c := s.client
	c.Scrollback, c.Keychain = true, fake.Store
	dir := filepath.Join(s.root, "saved")
	cwd := filepath.Join(s.root, "work dir")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(s.root, "print.sh")
	body := "#!/bin/sh\nprintf '" + scrollCanary + "-1 \\033[32mgreen\\033[0m \\303\\251\\n" + scrollCanary + "-2 touch \"" + filepath.Join(s.root, "must-not-run") + "\"\\n" + scrollCanary + "-END\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	var created createdSession
	mustCall(t, c, "session.create", map[string]any{"label": "scroll", "initial_windows": []createWindow{
		{Label: "plain", Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Label: "plain", Options: shellOptions{CWD: cwd}}}},
		{Label: "alt", Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Label: "alt", Options: shellOptions{CWD: cwd}}}},
	}}, &created)
	if len(created.Windows) != 2 {
		t.Fatalf("create: %+v", created)
	}
	plain, alt := created.Windows[0].Blocks[0], created.Windows[1].Blocks[0]
	for _, block := range []snapshot.LiveID{plain, alt} {
		waitDefaultTerminal(t, c, created.ID, block, cwd)
	}
	mustCall(t, c, "com.superlogical.terminal.write", map[string]any{"session_id": created.ID, "block_id": plain, "args": map[string]any{"data": "sh '" + script + "'\n"}}, nil)
	waitBlockText(t, c, created.ID, plain, scrollCanary+"-END")
	mustCall(t, c, "com.superlogical.terminal.write", map[string]any{"session_id": created.ID, "block_id": alt, "args": map[string]any{"data": "sh -c 'printf \"\\033[?1049h\"; sleep 120'\n"}}, nil)
	waitScreen(t, c, created.ID, alt, "alternate")
	r := commandReceipt(1, "claude", "echo")
	r.SessionID, r.BlockID = string(created.ID), string(plain)
	if err := c.SetReceipt(t.Context(), dir, r); err != nil {
		t.Fatal(err)
	}

	saved, err := c.Save(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Snapshot.Version != 4 {
		t.Fatalf("snapshot version %d", saved.Snapshot.Version)
	}
	var plainRef *snapshot.ScrollbackRef
	for _, b := range saved.Snapshot.Sessions[0].Blocks() {
		switch snapshot.LiveID(b.ID) {
		case plain:
			plainRef = b.Scrollback
		case alt:
			if b.Scrollback != nil {
				t.Fatal("alternate-screen block captured")
			}
		}
	}
	if plainRef == nil {
		t.Fatalf("plain block has no scrollback reference: %+v", saved.Report)
	}
	if len(saved.Report.ScrollbackSkipped) != 1 || saved.Report.ScrollbackSkipped[0].BlockID != snapshot.SourceID(alt) || saved.Report.ScrollbackSkipped[0].Reason != "alternate screen" {
		t.Fatalf("skips: %+v", saved.Report.ScrollbackSkipped)
	}
	sidecar := filepath.Join(dir, "scrollback", plainRef.KeyID, plainRef.ObjectID+".rrsb")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatal(err)
	}
	assertNoPlaintext(t, dir)
	// A manual save always publishes; unchanged content must reuse the
	// authenticated sidecar rather than seal a second one.
	again, err := c.Save(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range again.Snapshot.Sessions[0].Blocks() {
		if snapshot.LiveID(b.ID) == plain && (b.Scrollback == nil || *b.Scrollback != *plainRef) {
			t.Fatalf("unchanged scrollback changed its reference: %+v, want %+v", b.Scrollback, plainRef)
		}
	}
	if entries, err := os.ReadDir(filepath.Dir(sidecar)); err != nil || len(entries) != 1 {
		t.Fatalf("sidecars after an unchanged save: %v %v", entries, err)
	}

	s.stop(t)
	s.start(t)
	result, err := c.RestoreSaved(t.Context(), dir, 0)
	if err != nil || result.Outcome != "complete" || len(result.ScrollbackSkipped) != 0 {
		t.Fatalf("restore: %+v %v", result, err)
	}
	session, newPlain, newAlt := result.IDMap[saved.Snapshot.Sessions[0].ID], result.IDMap[snapshot.SourceID(plain)], result.IDMap[snapshot.SourceID(alt)]
	waitDefaultTerminal(t, c, session, newPlain, cwd)
	waitDefaultTerminal(t, c, session, newAlt, cwd)
	resumed := "--resume " + r.Conversation + " --model " + r.Model + " --effort " + r.Effort
	text := waitBlockText(t, c, session, newPlain, resumed)
	// The replay is the saved screen as it was (its own banner and prompts
	// included), followed by the new login banner and the agent command once.
	position := 0
	for _, want := range []string{"Last login", scrollCanary + "-1 green é", scrollCanary + "-2 touch", scrollCanary + "-END", "Last login", "> echo --resume", resumed} {
		at := strings.Index(text[position:], want)
		if at < 0 {
			t.Fatalf("replayed screen lacks %q after offset %d:\n%s", want, position, text)
		}
		position += at + len(want)
	}
	if strings.Count(text, resumed) != 1 || strings.Count(text, "Last login") != 2 {
		t.Fatalf("agent command or login banner repeated:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(s.root, "must-not-run")); err == nil {
		t.Fatal("replayed text was executed as input")
	}
	var vt struct {
		Content string `json:"content"`
	}
	if err := c.Call(t.Context(), "com.superlogical.terminal.format", map[string]any{"session_id": session, "block_id": newPlain, "args": map[string]any{"format": "vt"}}, &vt); err != nil || !strings.Contains(vt.Content, "38;5;2m") && !strings.Contains(vt.Content, "[32m") {
		t.Fatalf("replayed styling lost: %v", err)
	}
	if strings.Contains(blockText(t, c, session, newAlt), scrollCanary) {
		t.Fatal("skipped block received scrollback")
	}
	assertNoPlaintext(t, dir)
	if pins, err := os.ReadDir(filepath.Join(dir, "scrollback", "pins")); err != nil || len(pins) != 1 {
		t.Fatalf("restore did not pin its generation: %v %v", pins, err)
	}

	// New output, then the key disappears: the next restore degrades to plain
	// shells and says so, and nothing about the layout or receipt changes.
	mustCall(t, c, "com.superlogical.terminal.write", map[string]any{"session_id": session, "block_id": newPlain, "args": map[string]any{"data": "printf '" + scrollCanary + "-3\\n'\n"}}, nil)
	waitBlockText(t, c, session, newPlain, scrollCanary+"-3")
	saved2, err := c.Save(t.Context(), dir, false)
	if err != nil || saved2.Generation == 0 {
		t.Fatalf("second save: %+v %v", saved2, err)
	}
	fake.Remove(t, plainRef.KeyID)
	s.stop(t)
	s.start(t)
	result2, err := c.RestoreSaved(t.Context(), dir, 0)
	if err != nil || result2.Outcome != "complete" {
		t.Fatalf("restore without key: %+v %v", result2, err)
	}
	// Both blocks were on the primary screen by now, so both carried a
	// reference and both are reported.
	if len(result2.ScrollbackSkipped) == 0 {
		t.Fatalf("missing key not reported: %+v", result2.ScrollbackSkipped)
	}
	reported := false
	for _, skip := range result2.ScrollbackSkipped {
		if !strings.Contains(skip.Reason, "key missing") {
			t.Fatalf("unexpected skip: %+v", skip)
		}
		reported = reported || skip.BlockID == snapshot.SourceID(newPlain)
	}
	if !reported {
		t.Fatalf("plain block not among the skips: %+v", result2.ScrollbackSkipped)
	}
	session2, plain2 := result2.IDMap[saved2.Snapshot.Sessions[0].ID], result2.IDMap[snapshot.SourceID(newPlain)]
	waitDefaultTerminal(t, c, session2, plain2, cwd)
	text2 := waitBlockText(t, c, session2, plain2, resumed)
	if strings.Contains(text2, scrollCanary) {
		t.Fatalf("scrollback replayed without a key:\n%s", text2)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if saved3, err := c.Save(ctx, dir, false); err != nil || len(saved3.Report.ScrollbackSkipped) == 0 || !strings.Contains(saved3.Report.ScrollbackSkipped[0].Reason, "key missing") {
		t.Fatalf("save without key: %+v %v", saved3.Report.ScrollbackSkipped, err)
	}
	t.Logf("captured, sealed, replayed through the login chain with the agent command once, and degraded to plain shells without the key: %s", fmt.Sprint(len(text2) > 0))
}

// openState opens a fresh state store that closes with the test.
func openState(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

// scrollbackEnvelope is a saved layout with one session per shell block.
func scrollbackEnvelope(blocks ...snapshot.SourceID) state.Envelope {
	e := state.Envelope{HostID: "test-host", Snapshot: snapshot.Snapshot{Version: snapshot.Version}, Report: &snapshot.Report{}}
	for i, block := range blocks {
		session := snapshot.SourceID(fmt.Sprintf("session:%08d-89ab-4cde-8fab-0123456789ab", i))
		e.Snapshot.Sessions = append(e.Snapshot.Sessions, snapshot.Session{ID: session, ActiveWindow: "window",
			Windows: []snapshot.Window{{ID: "window", LayerID: "layer", Focus: block, Layout: snapshot.Node{Block: &snapshot.Block{ID: block, CWD: "/"}}}}})
	}
	return e
}

// A Rex read that fails is a per-block skip; only the caller's own
// cancellation stops a capture, and an empty capture provisions nothing.
func TestCaptureScrollbackDegradesReadFailures(t *testing.T) {
	fake := keychaintest.New(t, ScrollbackKeychainService)
	store := openState(t)
	c := Client{Binary: "/usr/bin/false", Socket: "/tmp/rexurrect-none.sock", Scrollback: true, Keychain: fake.Store}
	empty := scrollbackEnvelope()
	if err := c.captureScrollback(t.Context(), store, &empty); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(store.Dir(), "scrollback")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty capture created scrollback state: %v", err)
	}
	if items, err := os.ReadDir(fake.Items); err != nil || len(items) != 0 {
		t.Fatalf("empty capture provisioned a key: %v %v", items, err)
	}
	blocks := []snapshot.SourceID{"block:00000000-89ab-4cde-8fab-0123456789ab", "block:11111111-89ab-4cde-8fab-0123456789ab"}
	e := scrollbackEnvelope(blocks...)
	if err := c.captureScrollback(t.Context(), store, &e); err != nil {
		t.Fatalf("a failed read aborted the capture: %v", err)
	}
	if len(e.Report.ScrollbackSkipped) != len(blocks) {
		t.Fatalf("skips: %+v", e.Report.ScrollbackSkipped)
	}
	for i, skip := range e.Report.ScrollbackSkipped {
		if skip.BlockID != blocks[i] || !strings.HasPrefix(skip.Reason, "scrollback read: ") || len(skip.Reason) > 256 {
			t.Fatalf("skip %d: %+v", i, skip)
		}
	}
	for _, session := range e.Snapshot.Sessions {
		for _, b := range session.Blocks() {
			if b.Scrollback != nil {
				t.Fatalf("block %s has a reference without a sidecar", b.ID)
			}
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	e = scrollbackEnvelope(blocks[0])
	if err := c.captureScrollback(cancelled, store, &e); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture: %v", err)
	}
	slow := filepath.Join(t.TempDir(), "slow-rex")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.Binary = slow
	ctx, cancelDeadline := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancelDeadline()
	e = scrollbackEnvelope(blocks[0])
	if err := c.captureScrollback(ctx, store, &e); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capture past the deadline: %v", err)
	}
}

// sealedReplayFixture provisions the key, seals one block's scrollback into
// store, and points the replay at a stub executable.
func sealedReplayFixture(t *testing.T, store *state.Store, fake keychaintest.Fake) (Client, state.Envelope, snapshot.SourceID) {
	t.Helper()
	block := snapshot.SourceID("block:01234567-89ab-4cde-8fab-0123456789ab")
	e := scrollbackEnvelope(block)
	stateID, keyID, master, err := store.ScrollbackKey(t.Context(), fake.Store)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("earlier output\n")
	binding := scrollback.Binding{StateID: stateID, HostID: e.HostID, BlockID: string(block)}
	id, err := scrollback.Identity(master, binding, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	_, blob, err := scrollback.Seal(master, keyID, binding, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	ref := snapshot.ScrollbackRef{KeyID: keyID.String(), ObjectID: id.String()}
	if err := store.WriteScrollback(ref, blob); err != nil {
		t.Fatal(err)
	}
	e.Snapshot.Sessions[0].Blocks()[0].Scrollback = &ref
	previous := rexurrectExecutable
	rexurrectExecutable = func() (string, error) { return "/usr/bin/true", nil }
	t.Cleanup(func() { rexurrectExecutable = previous })
	return Client{Keychain: fake.Store}, e, block
}

// A pin that cannot be written turns every replay into a skip instead of
// failing the restore.
func TestScrollbackReplaySkipsWhenPinFails(t *testing.T) {
	fake := keychaintest.New(t, ScrollbackKeychainService)
	store := openState(t)
	c, e, block := sealedReplayFixture(t, store, fake)
	commands, skips, err := c.scrollbackReplay(t.Context(), store, e)
	if err != nil || len(commands) != 1 || len(skips) != 0 {
		t.Fatalf("replay decision: %d commands %+v %v", len(commands), skips, err)
	}
	pins := filepath.Join(store.Dir(), "scrollback", "pins")
	if err := os.RemoveAll(pins); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pins, []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	commands, skips, err = c.scrollbackReplay(t.Context(), store, e)
	if err != nil || commands != nil {
		t.Fatalf("pin failure did not degrade: %v %v", commands, err)
	}
	if len(skips) != 1 || skips[0].BlockID != block || !strings.HasPrefix(skips[0].Reason, "scrollback pin: ") {
		t.Fatalf("skips: %+v", skips)
	}
}

// The replay command runs in the restored block's cwd, so a relative state
// directory is embedded as an absolute path.
func TestScrollbackReplayResolvesRelativeStateDir(t *testing.T) {
	fake := keychaintest.New(t, ScrollbackKeychainService)
	t.Chdir(t.TempDir())
	store, err := state.Open("saved")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	c, e, block := sealedReplayFixture(t, store, fake)
	commands, skips, err := c.scrollbackReplay(t.Context(), store, e)
	if err != nil || len(skips) != 0 || len(commands) != 1 {
		t.Fatalf("replay decision: %d commands %+v %v", len(commands), skips, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	command := commands[block]
	if want := filepath.Join(wd, "saved"); slices.Contains(command, "saved") || !slices.Contains(command, want) {
		t.Fatalf("replay command embeds a relative state directory: %q", command)
	}
}

// A key load that fails because the restore was cancelled fails the replay
// decision instead of degrading to plain shells.
func TestScrollbackReplayPropagatesCancellation(t *testing.T) {
	fake := keychaintest.New(t, ScrollbackKeychainService)
	store := openState(t)
	c, e, _ := sealedReplayFixture(t, store, fake)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	commands, skips, err := c.scrollbackReplay(cancelled, store, e)
	if !errors.Is(err, context.Canceled) || commands != nil || skips != nil {
		t.Fatalf("cancelled replay decision: %v commands %+v skips %v", commands, skips, err)
	}
}

// Replay preparation covers only the sessions a restore has not attempted;
// the caller's envelope is left intact.
func TestCreatingSessionsKeepsOnlyUnattempted(t *testing.T) {
	e := scrollbackEnvelope("block:00000000-89ab-4cde-8fab-0123456789ab", "block:11111111-89ab-4cde-8fab-0123456789ab", "block:22222222-89ab-4cde-8fab-0123456789ab")
	complete, pending, configuring := e.Snapshot.Sessions[0].ID, e.Snapshot.Sessions[1].ID, e.Snapshot.Sessions[2].ID
	latest := map[snapshot.SourceID]state.JournalRecord{complete: {Outcome: "complete"}, configuring: {Outcome: "configuring"}}
	creating := creatingSessions(e, latest)
	if len(creating.Snapshot.Sessions) != 1 || creating.Snapshot.Sessions[0].ID != pending {
		t.Fatalf("creating sessions: %+v", creating.Snapshot.Sessions)
	}
	if len(e.Snapshot.Sessions) != 3 {
		t.Fatalf("caller's envelope was modified: %+v", e.Snapshot.Sessions)
	}
	if none := creatingSessions(e, map[snapshot.SourceID]state.JournalRecord{complete: {Outcome: "complete"}, pending: {Outcome: "created"}, configuring: {Outcome: "complete"}}); len(none.Snapshot.Sessions) != 0 {
		t.Fatalf("attempted sessions kept: %+v", none.Snapshot.Sessions)
	}
}

// A restore whose deadline expires while the scrollback key is loading fails
// before any create intent is recorded, so the next restore starts clean.
func TestRestoreCancelledDuringKeyLoadRecordsNoIntent(t *testing.T) {
	fake := keychaintest.New(t, ScrollbackKeychainService)
	fake.Timeout = 2 * time.Second
	t.Setenv("RR_FAKE_HANG", "1")
	binary := filepath.Join(t.TempDir(), "rex")
	if err := os.WriteFile(binary, []byte(`#!/bin/sh
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"new","rex_version":"v"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"v","commit":"c"}' ;;
  *) exit 1 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	c := Client{Binary: binary, Socket: "/tmp/rexurrect-none.sock", Keychain: fake.Store}
	store := openState(t)
	block := snapshot.SourceID("block:01234567-89ab-4cde-8fab-0123456789ab")
	e := scrollbackEnvelope(block)
	e.CapturedAt, e.RexVersion, e.Commit, e.HostID, e.InstanceID = time.Now().UTC(), "v", "c", "host", "saved"
	e.Snapshot.Sessions[0].Blocks()[0].Scrollback = &snapshot.ScrollbackRef{KeyID: strings.Repeat("ab", 16), ObjectID: strings.Repeat("cd", 32)}
	published, err := store.Publish(e, false)
	if err != nil {
		t.Fatal(err)
	}
	target := serverIdentity{HostID: "host", InstanceID: "new", RexVersion: "v", Commit: "c"}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	out, err := c.restoreSaved(ctx, store, published, target)
	if !errors.Is(err, context.DeadlineExceeded) || len(out.ScrollbackSkipped) != 0 {
		t.Fatalf("cancelled restore: %+v %v", out, err)
	}
	j, err := store.LoadJournal(published, target.HostID, target.InstanceID)
	if err != nil || len(j.Records) != 0 {
		t.Fatalf("cancelled restore recorded %d journal records (%v): %+v", len(j.Records), err, j.Records)
	}
}

// A resumed restore prepares replay only for the sessions it creates: a
// session completed by an earlier invocation is neither re-authenticated nor
// reported again.
func TestScrollbackReplayResumedRestorePrivateServer(t *testing.T) {
	fake := keychaintest.New(t, ScrollbackKeychainService)
	s := newIsolatedServer(t)
	c := s.client
	c.Scrollback, c.Keychain = true, fake.Store
	dir := filepath.Join(s.root, "saved")
	live := make(map[string]snapshot.SourceID)
	for _, label := range []string{"a", "b"} {
		var created createdSession
		mustCall(t, c, "session.create", map[string]any{"label": label, "initial_windows": []createWindow{{Label: label, Layout: createNode{Block: &createBlock{Flavor: shellFlavor, Label: label, Options: shellOptions{Shell: "/bin/sh", Login: new(bool), CWD: s.root}}}}}}, &created)
		block := created.Windows[0].Blocks[0]
		mustCall(t, c, "com.superlogical.terminal.write", map[string]any{"session_id": created.ID, "block_id": block, "args": map[string]any{"data": "printf '" + scrollCanary + "-" + label + "\\n'\n"}}, nil)
		waitBlockText(t, c, created.ID, block, scrollCanary+"-"+label)
		live[label] = snapshot.SourceID(created.ID)
	}
	saved, err := c.Save(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	var keyID string
	blocks := make(map[snapshot.SourceID]snapshot.SourceID)
	for _, session := range saved.Snapshot.Sessions {
		b := session.Blocks()
		if len(b) != 1 || b[0].Scrollback == nil {
			t.Fatalf("session %s has no scrollback reference: %+v", session.ID, saved.Report)
		}
		keyID = b[0].Scrollback.KeyID
		blocks[session.ID] = b[0].ID
	}
	sessionA, sessionB := live["a"], live["b"]
	// Without the key every replay is a skip, so the restored shells are
	// plain /bin/sh and no login shell is involved.
	fake.Remove(t, keyID)
	s.stop(t)
	s.start(t)
	first, err := c.RestoreSaved(t.Context(), dir, 0)
	if err != nil || first.Outcome != "complete" || len(first.ScrollbackSkipped) != 2 {
		t.Fatalf("first restore: %+v %v", first, err)
	}
	// Forget session b's journal records and its live session: the state a
	// crash between the two sessions leaves, a complete and b unattempted.
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	j, err := store.LoadJournal(saved, first.HostID, first.InstanceID)
	if err != nil {
		t.Fatal(errors.Join(err, store.Close()))
	}
	j.Records = slices.DeleteFunc(j.Records, func(r state.JournalRecord) bool { return r.SourceID == sessionB })
	if err := errors.Join(store.WriteJournal(saved, j), store.Close()); err != nil {
		t.Fatal(err)
	}
	mustCall(t, c, "session.destroy", map[string]any{"session_id": first.IDMap[sessionB]}, nil)
	second, err := c.RestoreSaved(t.Context(), dir, 0)
	if err != nil || second.Outcome != "complete" {
		t.Fatalf("resumed restore: %+v %v", second, err)
	}
	if len(second.ScrollbackSkipped) != 1 || second.ScrollbackSkipped[0].BlockID != blocks[sessionB] {
		t.Fatalf("resumed restore reported sessions it did not create: %+v", second.ScrollbackSkipped)
	}
	if second.IDMap[sessionA] != first.IDMap[sessionA] || second.IDMap[sessionB] == "" || second.IDMap[sessionB] == first.IDMap[sessionB] {
		t.Fatalf("resumed restore identities: first %v, second %v", first.IDMap, second.IDMap)
	}
}
