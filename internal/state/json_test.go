package state

import (
	"bytes"
	"strings"
	"testing"

	"github.com/blacktop/rexurrect/internal/receipt"
)

func TestStateRejectsDuplicateJSONMembers(t *testing.T) {
	s := openStore(t)
	e, err := s.Publish(fixture(), false)
	if err != nil {
		t.Fatal(err)
	}
	j := NewJournal(e, e.HostID, e.InstanceID)
	marker, err := NewOperationMarker()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(e, &j, "session", marker, "intent", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReceipt(e.HostID, stateReceipt(1)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, member string
		valid        []byte
		load         func() error
	}{
		{"receipts.json", `"model":"model-1"`, encoded(t, receiptFile{FormatVersion: ReceiptFormatVersion, HostID: e.HostID, Receipts: []receipt.Receipt{stateReceipt(1)}}), func() error { _, err := s.LoadReceipts(e.HostID); return err }},
		{generationName(e.Generation), `"source_id":"session"`, encoded(t, e), func() error { _, err := s.LoadCurrent(); return err }},
		{journalName(j.Generation, j.HostID, j.InstanceID), `"outcome":"intent"`, encoded(t, j), func() error { _, err := s.LoadJournal(e, e.HostID, e.InstanceID); return err }},
		{"current.json", `"generation":1`, encoded(t, pointer{Generation: 1}), func() error { _, err := s.LoadCurrent(); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := tc.member + "," + tc.member
			if tc.name == "receipts.json" {
				// The last model is valid: ordinary encoding/json would hide
				// the invalid first value from receipt validation.
				invalid = `"model":"bad;id","mo\u0064el":"model-1"`
			}
			crafted := bytes.Replace(tc.valid, []byte(tc.member), []byte(invalid), 1)
			if bytes.Equal(crafted, tc.valid) {
				t.Fatal("fixture did not change")
			}
			writeFile(t, s.dir, tc.name, crafted)
			if err := tc.load(); err == nil || !strings.Contains(err.Error(), "duplicate JSON object member") {
				t.Fatalf("duplicate accepted: %v", err)
			}
			writeFile(t, s.dir, tc.name, tc.valid)
			if err := tc.load(); err != nil {
				t.Fatalf("valid file: %v", err)
			}
		})
	}
}
