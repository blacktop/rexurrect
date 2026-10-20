package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	set := []string{"--session", session, "--block", block, "--harness", "claude-ddb", "--conversation", conversation, "--model", "model-1", "--effort", "high"}
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
	want := []receipt.Receipt{{Key: receipt.Key{SessionID: session, BlockID: block}, Harness: "claude-ddb", Conversation: conversation, Model: "model-1", Effort: "high"}}
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
	if _, err := invoke("clear", "--session", session, "--block", block, "--conversation", "00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	data, err = invoke("list", "--json")
	if err != nil || json.Unmarshal(data, &entries) != nil || !reflect.DeepEqual(entries, want) {
		t.Fatalf("stale clear removed current conversation: %s %v", data, err)
	}
	for _, miss := range [][]string{{"--harness", "codex"}, {"--harness", "codex", "--conversation", conversation}, {"--harness", "claude-ddb", "--conversation", "00000000-0000-0000-0000-000000000000"}} {
		if _, err := invoke("clear", append([]string{"--session", session, "--block", block}, miss...)...); err != nil {
			t.Fatal(err)
		}
		data, err = invoke("list", "--json")
		if err != nil || json.Unmarshal(data, &entries) != nil || !reflect.DeepEqual(entries, want) {
			t.Fatalf("guard %q removed the receipt: %s %v", miss, data, err)
		}
	}
	for _, bad := range [][]string{{"--harness", "zsh"}, {"--harness", "codex;id"}, {"--lock-timeout", "0"}, {"--lock-timeout", "61s"}} {
		if _, err := invoke("clear", append([]string{"--session", session, "--block", block}, bad...)...); err == nil {
			t.Fatalf("accepted clear %q", bad)
		}
	}
	if _, err := invoke("set", append(set, "--lock-timeout", "0")...); err == nil {
		t.Fatal("accepted set --lock-timeout 0")
	}
	if _, err := invoke("set", append(set, "--harness", "codex", "--lock-timeout", "61s")...); err == nil {
		t.Fatal("accepted set --lock-timeout 61s")
	}
	if _, err := invoke("list", "--lock-timeout", "1s"); err == nil {
		t.Fatal("list accepted --lock-timeout")
	}
	if _, err := invoke("clear", "--session", session, "--block", block, "--harness", "claude-ddb", "--conversation", conversation, "--lock-timeout", "1s"); err != nil {
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
}
