// Package scrollback turns captured terminal output into a bounded, sanitized,
// compressed, authenticated-encrypted blob and back. The byte subset and the
// format follow docs/.ai/plans/2026-10-21-scrollback-encryption.md and the
// probes in docs/.ai/evidence/2026-10-21-scrollback-contract-probe.md.
package scrollback

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"unicode/utf8"
)

const (
	// MaxPlaintext bounds one block's sanitized scrollback.
	MaxPlaintext = 1 << 20
	// KeySize is the master key length the keychain store provides.
	KeySize = 32

	magic          = "RRSBLOB1"
	headerSize     = 8 + 16 + 32 + 32 + 4 // magic, key ID, object ID, salt, plaintext length
	aeadOverhead   = 28                   // GCM random nonce and tag
	maxCompressed  = MaxPlaintext + 1024  // zlib stored blocks add at most a few bytes per 64 KiB
	bindingDomain  = "rexurrect.scrollback.binding.v1\x00"
	identityDomain = "rexurrect.scrollback.id.v1"
	cipherDomain   = "rexurrect.scrollback.encryption.v1"
)

// KeyID names a master key in the keychain store.
type KeyID [16]byte

// ObjectID is the keyed identity of one block's sanitized scrollback under
// its binding; equal content under the same binding has the same ID.
type ObjectID [32]byte

func (k KeyID) String() string    { return hex.EncodeToString(k[:]) }
func (o ObjectID) String() string { return hex.EncodeToString(o[:]) }

// ParseKeyID reads the 32-hex-digit form the keychain store uses.
func ParseKeyID(s string) (KeyID, error) {
	var k KeyID
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != len(k) || hex.EncodeToString(raw) != s {
		return k, errors.New("key ID must be 32 lowercase hex digits")
	}
	copy(k[:], raw)
	return k, nil
}

// ParseObjectID reads the 64-hex-digit form stored in snapshot references.
func ParseObjectID(s string) (ObjectID, error) {
	var o ObjectID
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != len(o) || hex.EncodeToString(raw) != s {
		return o, errors.New("object ID must be 64 lowercase hex digits")
	}
	copy(o[:], raw)
	return o, nil
}

// Binding ties a blob to one state store, Rex host, and saved source block, so
// a blob cannot be replayed into another block or another installation.
type Binding struct {
	StateID [16]byte
	HostID  string
	BlockID string
}

func (b Binding) encode() ([]byte, error) {
	if len(b.HostID) == 0 || len(b.HostID) > 256 || len(b.BlockID) == 0 || len(b.BlockID) > 256 {
		return nil, errors.New("scrollback binding needs a bounded host and block ID")
	}
	out := append([]byte(bindingDomain), b.StateID[:]...)
	for _, value := range []string{b.HostID, b.BlockID} {
		out = binary.BigEndian.AppendUint16(out, uint16(len(value)))
		out = append(out, value...)
	}
	return out, nil
}

// Sanitize keeps the replayable subset Rex's vt format was measured to emit:
// printable UTF-8, CR, LF, and SGR sequences (ESC [ params m). Every other
// escape sequence, control byte, C1 character, or invalid byte is dropped.
// The second result counts dropped sequences and bytes.
func Sanitize(vt []byte) ([]byte, int) {
	out := make([]byte, 0, len(vt))
	removed := 0
	for i := 0; i < len(vt); {
		c := vt[i]
		switch {
		case c == 0x1b:
			n, keep := escapeLength(vt[i:])
			if keep {
				out = append(out, vt[i:i+n]...)
			} else {
				removed++
			}
			i += n
		case c == '\r' || c == '\n':
			out = append(out, c)
			i++
		case c < 0x20 || c == 0x7f:
			removed++
			i++
		case c < utf8.RuneSelf:
			out = append(out, c)
			i++
		default:
			r, size := utf8.DecodeRune(vt[i:])
			if r == utf8.RuneError && size <= 1 || r >= 0x80 && r <= 0x9f {
				removed++
			} else {
				out = append(out, vt[i:i+size]...)
			}
			i += size
		}
	}
	return out, removed
}

// escapeLength returns how many bytes the escape sequence at the start of
// data occupies and whether it is an SGR sequence worth keeping. Sequences cut
// off by the end of data are consumed whole and dropped.
func escapeLength(data []byte) (int, bool) {
	if len(data) < 2 {
		return len(data), false
	}
	switch data[1] {
	case '[':
		i := 2
		for i < len(data) && (data[i] >= '0' && data[i] <= '9' || data[i] == ';' || data[i] == ':') {
			i++
		}
		if i < len(data) && data[i] == 'm' {
			return i + 1, true
		}
		for i < len(data) && (data[i] < 0x40 || data[i] > 0x7e) {
			i++
		}
		if i < len(data) {
			i++
		}
		return i, false
	case ']', 'P', '_', '^', 'X':
		for i := 2; i < len(data); i++ {
			if data[i] == 0x07 {
				return i + 1, false
			}
			if data[i] == 0x1b && i+1 < len(data) && data[i+1] == '\\' {
				return i + 2, false
			}
		}
		return len(data), false
	default:
		return 2, false
	}
}

func deriveKey(master, salt []byte, domain string) ([]byte, error) {
	if len(master) != KeySize {
		return nil, errors.New("scrollback master key must be 32 bytes")
	}
	return hkdf.Key(sha256.New, master, salt, domain, KeySize)
}

// Identity is the object ID of plaintext under b: an HMAC with a key derived
// from the master key, so unchanged saves can be detected without publishing a
// plain content hash.
func Identity(master []byte, b Binding, plaintext []byte) (ObjectID, error) {
	var id ObjectID
	context, err := b.encode()
	if err != nil {
		return id, err
	}
	key, err := deriveKey(master, nil, identityDomain)
	if err != nil {
		return id, err
	}
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(context)
	_, _ = mac.Write(plaintext)
	copy(id[:], mac.Sum(nil))
	return id, nil
}

func aead(master, salt []byte) (cipher.AEAD, error) {
	key, err := deriveKey(master, salt, cipherDomain)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// Seal compresses sanitized plaintext and encrypts it under a key derived from
// master and a fresh salt, authenticating the header and binding. The
// plaintext must already be in the replayable subset; anything else is
// refused rather than cleaned here, so callers cannot skip Sanitize by
// accident.
func Seal(master []byte, keyID KeyID, b Binding, plaintext []byte) (ObjectID, []byte, error) {
	var zero ObjectID
	if len(plaintext) > MaxPlaintext {
		return zero, nil, errors.New("scrollback exceeds 1 MiB")
	}
	if clean, removed := Sanitize(plaintext); removed != 0 || !bytes.Equal(clean, plaintext) {
		return zero, nil, errors.New("scrollback plaintext is not sanitized")
	}
	context, err := b.encode()
	if err != nil {
		return zero, nil, err
	}
	id, err := Identity(master, b, plaintext)
	if err != nil {
		return zero, nil, err
	}
	var compressed bytes.Buffer
	w, err := zlib.NewWriterLevel(&compressed, zlib.DefaultCompression)
	if err != nil {
		return zero, nil, err
	}
	if _, err := w.Write(plaintext); err != nil {
		return zero, nil, err
	}
	if err := w.Close(); err != nil {
		return zero, nil, err
	}
	out := make([]byte, headerSize, headerSize+compressed.Len()+aeadOverhead)
	copy(out, magic)
	copy(out[8:24], keyID[:])
	copy(out[24:56], id[:])
	if _, err := rand.Read(out[56:88]); err != nil {
		return zero, nil, err
	}
	binary.BigEndian.PutUint32(out[88:92], uint32(len(plaintext)))
	box, err := aead(master, out[56:88])
	if err != nil {
		return zero, nil, err
	}
	additional := slices.Concat(out, context)
	return id, box.Seal(out, nil, compressed.Bytes(), additional), nil
}

// Open authenticates blob against the expected key ID, object ID, and binding,
// decrypts, decompresses within the authenticated length, and re-checks the
// identity and the byte subset. Nothing is returned unless every check passes.
func Open(master []byte, keyID KeyID, want ObjectID, b Binding, blob []byte) ([]byte, error) {
	if len(blob) < headerSize+aeadOverhead || len(blob) > headerSize+aeadOverhead+maxCompressed {
		return nil, errors.New("scrollback blob has an invalid length")
	}
	if string(blob[:8]) != magic || !hmac.Equal(blob[8:24], keyID[:]) || !hmac.Equal(blob[24:56], want[:]) {
		return nil, errors.New("scrollback blob identity mismatch")
	}
	length := binary.BigEndian.Uint32(blob[88:92])
	if length > MaxPlaintext {
		return nil, errors.New("scrollback blob declares an oversized plaintext")
	}
	context, err := b.encode()
	if err != nil {
		return nil, err
	}
	box, err := aead(master, blob[56:88])
	if err != nil {
		return nil, err
	}
	additional := slices.Concat(blob[:headerSize], context)
	compressed, err := box.Open(nil, nil, blob[headerSize:], additional)
	if err != nil {
		return nil, errors.New("scrollback blob authentication failed")
	}
	plaintext, err := inflate(compressed, int(length))
	clear(compressed)
	if err != nil {
		return nil, err
	}
	id, err := Identity(master, b, plaintext)
	if err != nil || !hmac.Equal(id[:], want[:]) {
		clear(plaintext)
		return nil, errors.New("scrollback plaintext identity mismatch")
	}
	if clean, removed := Sanitize(plaintext); removed != 0 || !bytes.Equal(clean, plaintext) {
		clear(plaintext)
		return nil, errors.New("scrollback plaintext left the replayable subset")
	}
	return plaintext, nil
}

// inflate decompresses exactly length bytes; shorter, longer, or trailing data
// is an error, so a tampered or bomb-shaped stream never expands past the
// authenticated length.
func inflate(compressed []byte, length int) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, errors.New("scrollback blob is not a zlib stream")
	}
	plaintext := make([]byte, length+1)
	n, err := io.ReadFull(r, plaintext)
	if closeErr := r.Close(); closeErr != nil {
		err = closeErr
	}
	if n != length || !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		clear(plaintext)
		return nil, errors.New("scrollback blob length does not match its header")
	}
	return plaintext[:length], nil
}
