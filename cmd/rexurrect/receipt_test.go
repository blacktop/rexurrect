package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blacktop/rexurrect/internal/receipt"
)

func TestReceiptCLI(t *testing.T) {
	root := t.TempDir()
	binary, dir := filepath.Join(root, "rex"), filepath.Join(root, "saved")
	session := "session:01234567-89ab-4cde-8fab-0123456789ab"
	block := "block:01234567-89ab-4cde-8fab-0123456789ab"
	conversation := "01234567-89ab-4cde-8fab-0123456789ab"
	script := `#!/bin/sh
case "$6:$8" in
  whoami:*) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  api:server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  api:session.list_blocks) printf '%s\n' '{"blocks":[{"block_id":"` + block + `","flavor":"com.superlogical.terminal.shell"}]}' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	base := []string{"--server", "unix:///private/tmp/receipt-cli-unused", "--state-dir", dir, "--rex", binary}
	invoke := func(action string, extra ...string) ([]byte, error) {
		var out, diagnostic bytes.Buffer
		args := append(append([]string{"receipt", action}, base...), extra...)
		err := run(t.Context(), args, &out, &diagnostic)
		return out.Bytes(), err
	}
	set := []string{"--session", session, "--block", block, "--host", "claude", "--command", "claude-work", "--conversation", conversation, "--model", "model-1", "--effort", "high"}
	if _, err := invoke("set", set...); err != nil {
		t.Fatal(err)
	}
	data, err := invoke("list", "--json", "--session", session)
	var entries []receipt.Receipt
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	want := []receipt.Receipt{{SessionID: session, BlockID: block, Host: "claude", Command: "claude-work", Conversation: conversation, Model: "model-1", Effort: "high"}}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("list: %s", data)
	}
	for _, args := range [][]string{{"--call-timeout", "0"}, {"--json", "--block", "bad"}} {
		if _, err := invoke("list", args...); err == nil {
			t.Fatalf("accepted list %q", args)
		}
	}
	if _, err := invoke("set", append(set, "--effort", "high;id")...); err == nil {
		t.Fatal("accepted injection")
	}
	for _, bad := range [][]string{
		{"--host", "vim"}, {"--host", ""}, {"--command", ""}, {"--command", "-claude"},
		{"--command", "claude work"}, {"--command", strings.Repeat("c", 65)},
		{"--command", "if"},
		{"--harness", "claude"},
	} {
		if _, err := invoke("set", append(set, bad...)...); err == nil {
			t.Fatalf("accepted set %q", bad)
		}
	}
	if _, err := invoke("set", append(set, "--command", "Claude_Work.v2")...); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke("set", set...); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke("clear", "--session", session, "--block", block, "--conversation", "00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	data, err = invoke("list", "--json")
	if err != nil || json.Unmarshal(data, &entries) != nil || !reflect.DeepEqual(entries, want) {
		t.Fatalf("stale clear removed current conversation: %s %v", data, err)
	}
	key := []string{"--session", session, "--block", block}
	for _, miss := range [][]string{
		{"--command", "codex"},
		{"--harness", "codex"},
		{"--command", "codex", "--conversation", conversation},
		{"--command", "claude-work", "--conversation", "00000000-0000-0000-0000-000000000000"},
	} {
		if _, err := invoke("clear", append(append([]string{}, key...), miss...)...); err != nil {
			t.Fatal(err)
		}
		data, err = invoke("list", "--json")
		if err != nil || json.Unmarshal(data, &entries) != nil || !reflect.DeepEqual(entries, want) {
			t.Fatalf("guard %q removed the receipt: %s %v", miss, data, err)
		}
	}
	for _, bad := range [][]string{
		{"--command", "-zsh"},
		{"--command", "codex;id"},
		{"--command", "claude-work", "--harness", "codex"},
		{"--lock-timeout", "0"},
		{"--lock-timeout", "61s"},
	} {
		if _, err := invoke("clear", append(append([]string{}, key...), bad...)...); err == nil {
			t.Fatalf("accepted clear %q", bad)
		}
	}
	if _, err := invoke("set", append(set, "--lock-timeout", "0")...); err == nil {
		t.Fatal("accepted set --lock-timeout 0")
	}
	over := append(set, "--command", "codex", "--lock-timeout", "61s")
	if _, err := invoke("set", over...); err == nil {
		t.Fatal("accepted set --lock-timeout 61s")
	}
	if _, err := invoke("list", "--lock-timeout", "1s"); err == nil {
		t.Fatal("list accepted --lock-timeout")
	}
	guarded := append(append([]string{}, key...),
		"--command", "claude-work", "--conversation", conversation, "--lock-timeout", "1s")
	if _, err := invoke("clear", guarded...); err != nil {
		t.Fatal(err)
	}
	data, err = invoke("list", "--json")
	if err != nil || string(data) != "[]\n" {
		t.Fatalf("guarded clear/list: %s %v", data, err)
	}
	if _, err := invoke("set", set...); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke("clear", "--session", session, "--block", block); err != nil {
		t.Fatal(err)
	}
	data, err = invoke("list", "--json")
	if err != nil || string(data) != "[]\n" {
		t.Fatalf("clear/list: %s %v", data, err)
	}
	legacy := append(append([]string{}, key...), "--harness", "codex", "--conversation", conversation)
	if _, err := invoke("set", legacy...); err != nil {
		t.Fatalf("released set flags: %v", err)
	}
	data, err = invoke("list", "--json")
	want = []receipt.Receipt{{SessionID: session, BlockID: block, Harness: "codex", Conversation: conversation}}
	entries = nil
	if err != nil || json.Unmarshal(data, &entries) != nil || !reflect.DeepEqual(entries, want) {
		t.Fatalf("released set/list: %s %v", data, err)
	}
	if _, err := invoke("clear", append(append([]string{}, key...), "--harness", "codex", "--conversation", conversation)...); err != nil {
		t.Fatalf("released clear flags: %v", err)
	}
	data, err = invoke("list", "--json")
	if err != nil || string(data) != "[]\n" {
		t.Fatalf("released clear/list: %s %v", data, err)
	}
}
