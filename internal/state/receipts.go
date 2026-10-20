package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/blacktop/rexurrect/internal/receipt"
)

const (
	MaxReceipts     = 1024
	MaxReceiptsSize = 512 << 10
)

type receiptFile struct {
	FormatVersion int               `json:"format_version"`
	HostID        string            `json:"host_id"`
	Receipts      []receipt.Receipt `json:"receipts"`
}

func validateReceipts(host string, entries []receipt.Receipt) error {
	if !bounded(host, 256) || len(entries) > MaxReceipts {
		return errors.New("invalid receipt host or count")
	}
	seen := make(map[receipt.Key]bool, len(entries))
	for _, r := range entries {
		if err := r.Validate(); err != nil {
			return err
		}
		data, err := json.Marshal(r)
		if err != nil || len(data) > receipt.MaxSize || seen[r.Key] {
			return errors.New("duplicate or oversized receipt")
		}
		seen[r.Key] = true
	}
	return nil
}

func (s *Store) LoadReceipts(host string) ([]receipt.Receipt, error) {
	var f receiptFile
	if err := s.read("receipts.json", MaxReceiptsSize, &f); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []receipt.Receipt{}, nil
		}
		return nil, err
	}
	if f.FormatVersion != FormatVersion || f.HostID != host || f.Receipts == nil {
		return nil, errors.New("invalid receipt store binding")
	}
	return f.Receipts, validateReceipts(host, f.Receipts)
}

func (s *Store) WriteReceipts(host string, entries []receipt.Receipt) error {
	if err := validateReceipts(host, entries); err != nil {
		return err
	}
	entries = append([]receipt.Receipt{}, entries...)
	slices.SortFunc(entries, func(a, b receipt.Receipt) int {
		if n := strings.Compare(a.SessionID, b.SessionID); n != 0 {
			return n
		}
		return strings.Compare(a.BlockID, b.BlockID)
	})
	f := receiptFile{FormatVersion: FormatVersion, HostID: host, Receipts: entries}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(data) >= MaxReceiptsSize {
		return fmt.Errorf("receipts exceed %d-byte size limit", MaxReceiptsSize)
	}
	return s.write("receipts.json", f)
}

func (s *Store) SetReceipt(host string, r receipt.Receipt) error {
	if err := r.Validate(); err != nil {
		return err
	}
	entries, err := s.LoadReceipts(host)
	if err != nil {
		return err
	}
	for i, existing := range entries {
		if existing.Key == r.Key {
			if existing == r {
				return nil
			}
			entries[i] = r
			return s.WriteReceipts(host, entries)
		}
	}
	return s.WriteReceipts(host, append(entries, r))
}

func (s *Store) ClearReceipt(host string, key receipt.Key, guard receipt.ClearGuard) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if err := guard.Validate(); err != nil {
		return err
	}
	entries, err := s.LoadReceipts(host)
	if err != nil {
		return err
	}
	for i, r := range entries {
		if r.Key == key && guard.Matches(r) {
			return s.WriteReceipts(host, slices.Delete(entries, i, i+1))
		}
	}
	return nil
}
