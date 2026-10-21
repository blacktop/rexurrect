package scrollback

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canary = "REXURRECT_NONSECRET_CANARY"

func fixture(t *testing.T) ([]byte, KeyID, Binding, []byte) {
	t.Helper()
	key := make([]byte, KeySize)
	var keyID KeyID
	b := Binding{HostID: "test-host", BlockID: "block:01234567-89ab-4cde-8fab-0123456789ab"}
	for _, value := range [][]byte{key, keyID[:], b.StateID[:]} {
		if _, err := rand.Read(value); err != nil {
			t.Fatal(err)
		}
	}
	text := []byte(canary + " é 中 😀\r\n\x1b[38;5;2mgreen\x1b[0m \x1b[1;44mbold\x1b[0m\r\n")
	return key, keyID, b, text
}

func TestSanitizeKeepsOnlyTheReplayableSubset(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		removed        int
	}{
		{"plain", "hello\r\nworld\n", "hello\r\nworld\n", 0},
		{"utf8", "é 中 😀", "é 中 😀", 0},
		{"sgr", "\x1b[38;2;10;200;30mtrue\x1b[0m \x1b[4;7m\x1b[m", "\x1b[38;2;10;200;30mtrue\x1b[0m \x1b[4;7m\x1b[m", 0},
		{"sgr colon", "\x1b[4:3mcurly\x1b[0m", "\x1b[4:3mcurly\x1b[0m", 0},
		{"osc clipboard", "a\x1b]52;c;SGVsbG8=\x07b", "ab", 1},
		{"osc title st", "a\x1b]0;title\x1b\\b", "ab", 1},
		{"osc hyperlink", "\x1b]8;;https://x/\x1b\\link\x1b]8;;\x1b\\", "link", 2},
		{"dcs", "a\x1bP+q544e\x1b\\b", "ab", 1},
		{"apc pm sos", "a\x1b_x\x1b\\b\x1b^y\x1b\\c\x1bXz\x1b\\d", "abcd", 3},
		{"csi cursor and erase", "a\x1b[H\x1b[2J\x1b[3J\x1b[1;5Hb", "ab", 4},
		{"csi private mode", "a\x1b[?1049h\x1b[?2004lb", "ab", 2},
		{"csi queries", "a\x1b[c\x1b[6n\x1b[>cb", "ab", 3},
		{"csi with intermediate", "a\x1b[ qb", "ab", 1},
		{"csi sgr private marker", "a\x1b[?5mb", "ab", 1},
		{"two byte escapes", "a\x1bMb\x1b7c\x1b8d\x1bcx", "abcdx", 4},
		{"c0 and del", "a\x07b\x08c\td\x0ce\x7ff\x00g", "abcdefg", 6},
		{"c1", "a\u0085b\u009bc", "abc", 2},
		{"invalid utf8", "a\xffb\xc3c", "abc", 2},
		{"truncated escape", "abc\x1b[31", "abc", 1},
		{"truncated osc", "abc\x1b]0;never ends", "abc", 1},
		{"lone esc", "abc\x1b", "abc", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, removed := Sanitize([]byte(tc.in))
			if string(got) != tc.want || removed != tc.removed {
				t.Fatalf("Sanitize(%q) = %q, %d; want %q, %d", tc.in, got, removed, tc.want, tc.removed)
			}
			again, removedAgain := Sanitize(got)
			if !bytes.Equal(again, got) || removedAgain != 0 {
				t.Fatalf("Sanitize is not idempotent on %q", got)
			}
		})
	}
}

func TestRoundTripAndStableIdentity(t *testing.T) {
	key, keyID, b, text := fixture(t)
	id, first, err := Seal(key, keyID, b, text)
	if err != nil {
		t.Fatal(err)
	}
	id2, second, err := Seal(key, keyID, b, text)
	if err != nil || id != id2 || bytes.Equal(first, second) || bytes.Contains(first, []byte(canary)) {
		t.Fatalf("identity/randomization/confidentiality contract: %v", err)
	}
	want, err := Identity(key, b, text)
	if err != nil || want != id {
		t.Fatalf("Identity differs from Seal: %v", err)
	}
	got, err := Open(key, keyID, id, b, first)
	if err != nil || !bytes.Equal(got, text) {
		t.Fatalf("round trip: %v", err)
	}
	if id.String() == "" || keyID.String() == "" {
		t.Fatal("string forms")
	}
	parsed, err := ParseObjectID(id.String())
	if err != nil || parsed != id {
		t.Fatalf("ParseObjectID: %v", err)
	}
	parsedKey, err := ParseKeyID(keyID.String())
	if err != nil || parsedKey != keyID {
		t.Fatalf("ParseKeyID: %v", err)
	}
	for _, bad := range []string{"", "zz", strings.Repeat("0", 31), strings.Repeat("0", 33), strings.ToUpper(id.String()), strings.ToUpper(keyID.String())} {
		if _, err := ParseKeyID(bad); err == nil {
			t.Errorf("ParseKeyID accepted %q", bad)
		}
		if _, err := ParseObjectID(bad); err == nil {
			t.Errorf("ParseObjectID accepted %q", bad)
		}
	}
	other := b
	other.BlockID += "-other"
	id3, _, err := Seal(key, keyID, other, text)
	if err != nil || id == id3 {
		t.Fatalf("identity is not block-bound: %v", err)
	}
	empty, blob, err := Seal(key, keyID, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Open(key, keyID, empty, b, blob); err != nil || len(got) != 0 {
		t.Fatalf("empty round trip: %q %v", got, err)
	}
}

func TestCompressionIsApplied(t *testing.T) {
	key, keyID, b, _ := fixture(t)
	text := bytes.Repeat([]byte("\x1b[32mok\x1b[0m repeated scrollback line\r\n"), 20000)
	if len(text) > MaxPlaintext {
		t.Fatalf("fixture too large: %d", len(text))
	}
	id, blob, err := Seal(key, keyID, b, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) > len(text)/10 {
		t.Fatalf("blob %d bytes for %d bytes of repetitive text; compression missing", len(blob), len(text))
	}
	got, err := Open(key, keyID, id, b, blob)
	if err != nil || !bytes.Equal(got, text) {
		t.Fatalf("compressed round trip: %v", err)
	}
	full := bytes.Repeat([]byte("x"), MaxPlaintext)
	if _, _, err := Seal(key, keyID, b, full); err != nil {
		t.Fatalf("1 MiB plaintext rejected: %v", err)
	}
	if _, _, err := Seal(key, keyID, b, append(full, 'x')); err == nil {
		t.Fatal("accepted plaintext above 1 MiB")
	}
}

func TestSealRefusesUnsanitizedInput(t *testing.T) {
	key, keyID, b, _ := fixture(t)
	for _, bad := range []string{"a\x1b]52;c;SGVsbG8=\x07", "a\x1b[2J", "a\tb", "a\xff", "a\x1b[?1049h"} {
		if _, _, err := Seal(key, keyID, b, []byte(bad)); err == nil {
			t.Errorf("Seal accepted %q", bad)
		}
	}
	short := bytes.Repeat([]byte{1}, KeySize-1)
	if _, _, err := Seal(short, keyID, b, []byte("ok")); err == nil {
		t.Error("Seal accepted a short key")
	}
	if _, err := Identity(short, b, []byte("ok")); err == nil {
		t.Error("Identity accepted a short key")
	}
	for _, bad := range []Binding{{HostID: "", BlockID: "b"}, {HostID: "h", BlockID: ""}, {HostID: strings.Repeat("h", 257), BlockID: "b"}} {
		if _, _, err := Seal(key, keyID, bad, []byte("ok")); err == nil {
			t.Errorf("Seal accepted binding %+v", bad)
		}
	}
}

func TestRejectTamperingAndWrongContext(t *testing.T) {
	key, keyID, b, text := fixture(t)
	id, blob, err := Seal(key, keyID, b, text)
	if err != nil {
		t.Fatal(err)
	}
	for index := range blob {
		changed := bytes.Clone(blob)
		changed[index] ^= 1
		if plaintext, err := Open(key, keyID, id, b, changed); err == nil || len(plaintext) != 0 {
			t.Fatalf("accepted mutation at byte %d", index)
		}
	}
	wrongKey := bytes.Clone(key)
	wrongKey[0] ^= 1
	wrongID, wrongKeyID, wrongBinding := id, keyID, b
	wrongID[0] ^= 1
	wrongKeyID[0] ^= 1
	wrongBinding.StateID[0] ^= 1
	otherBlock := b
	otherBlock.BlockID = "block:01234567-89ab-4cde-8fab-0123456789ac"
	otherHost := b
	otherHost.HostID = "other-host"
	for name, tc := range map[string]struct {
		key     []byte
		keyID   KeyID
		id      ObjectID
		binding Binding
	}{
		"wrong key": {wrongKey, keyID, id, b}, "wrong key ID": {key, wrongKeyID, id, b},
		"wrong object ID": {key, keyID, wrongID, b}, "wrong state ID": {key, keyID, id, wrongBinding},
		"other block": {key, keyID, id, otherBlock}, "other host": {key, keyID, id, otherHost},
	} {
		if plaintext, err := Open(tc.key, tc.keyID, tc.id, tc.binding, blob); err == nil || len(plaintext) != 0 {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestBoundsBombsAndInterruptedWrites(t *testing.T) {
	key, keyID, b, text := fixture(t)
	id, blob, err := Seal(key, keyID, b, text)
	if err != nil {
		t.Fatal(err)
	}
	oversized := bytes.Clone(blob)
	binary.BigEndian.PutUint32(oversized[88:92], MaxPlaintext+1)
	for name, bad := range map[string][]byte{
		"nil": nil, "truncated": blob[:len(blob)-1], "extended": append(bytes.Clone(blob), 0),
		"oversized header": oversized, "too long": make([]byte, headerSize+aeadOverhead+maxCompressed+1),
	} {
		if plaintext, err := Open(key, keyID, id, b, bad); err == nil || len(plaintext) != 0 {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A valid encryption of a bomb-shaped stream: 64 MiB of zeros compressed,
	// sealed by hand with a header claiming 1 KiB. The reader must stop at the
	// authenticated length instead of inflating 64 MiB.
	var bomb bytes.Buffer
	w, _ := zlib.NewWriterLevel(&bomb, zlib.BestCompression)
	_, _ = w.Write(make([]byte, 64<<20))
	_ = w.Close()
	forged := forge(t, key, keyID, b, ObjectID{}, bomb.Bytes(), 1024)
	if plaintext, err := Open(key, keyID, forgedID(forged), b, forged); err == nil || len(plaintext) != 0 {
		t.Fatal("bomb expanded past its header length")
	}
	// A stream shorter than its header length is rejected too.
	var short bytes.Buffer
	w, _ = zlib.NewWriterLevel(&short, zlib.DefaultCompression)
	_, _ = w.Write([]byte("abc"))
	_ = w.Close()
	forgedShort := forge(t, key, keyID, b, ObjectID{}, short.Bytes(), 4)
	if plaintext, err := Open(key, keyID, forgedID(forgedShort), b, forgedShort); err == nil || len(plaintext) != 0 {
		t.Fatal("short stream accepted")
	}
	// A well-formed stream whose content does not match the header's object
	// ID: only the post-decryption identity check catches this.
	var swapped bytes.Buffer
	w, _ = zlib.NewWriterLevel(&swapped, zlib.DefaultCompression)
	_, _ = w.Write([]byte("abcd"))
	_ = w.Close()
	forgedSwap := forge(t, key, keyID, b, ObjectID{}, swapped.Bytes(), 4)
	if plaintext, err := Open(key, keyID, forgedID(forgedSwap), b, forgedSwap); err == nil || len(plaintext) != 0 {
		t.Fatal("content with a mismatched identity accepted")
	}
	// A stream longer than the header length, whose header identity matches
	// the prefix: only the exact-length read rejects the trailing bytes.
	prefixID, err := Identity(key, b, []byte("abcd"))
	if err != nil {
		t.Fatal(err)
	}
	var longer bytes.Buffer
	w, _ = zlib.NewWriterLevel(&longer, zlib.DefaultCompression)
	_, _ = w.Write([]byte("abcde"))
	_ = w.Close()
	forgedLong := forge(t, key, keyID, b, prefixID, longer.Bytes(), 4)
	if plaintext, err := Open(key, keyID, prefixID, b, forgedLong); err == nil || len(plaintext) != 0 {
		t.Fatal("stream longer than its header length accepted")
	}
	root := t.TempDir()
	for name, data := range map[string][]byte{"blob.rrsb": blob, ".interrupted": blob[:len(blob)/2]} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		stored, err := os.ReadFile(path)
		if err != nil || bytes.Contains(stored, []byte(canary)) || bytes.Contains(stored, []byte("green")) {
			t.Fatalf("plaintext reached disk: %v", err)
		}
	}
}

// forge seals an arbitrary compressed payload with a chosen header length,
// standing in for an attacker holding the key who edits a blob's contents.
func forge(t *testing.T, key []byte, keyID KeyID, b Binding, id ObjectID, compressed []byte, length uint32) []byte {
	t.Helper()
	context, err := b.encode()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, headerSize, headerSize+len(compressed)+aeadOverhead)
	copy(out, magic)
	copy(out[8:24], keyID[:])
	copy(out[24:56], id[:])
	if _, err := rand.Read(out[56:88]); err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(out[88:92], length)
	box, err := aead(key, out[56:88])
	if err != nil {
		t.Fatal(err)
	}
	additional := append(append([]byte{}, out...), context...)
	return box.Seal(out, nil, compressed, additional)
}

func forgedID(blob []byte) ObjectID {
	var id ObjectID
	copy(id[:], blob[24:56])
	return id
}
