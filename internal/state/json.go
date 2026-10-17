package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The standard decoder accepts repeated names and keeps the last value.
// Check every object before decoding into a typed state file.
func rejectDuplicateMembers(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 256 {
			return errors.New("JSON nesting exceeds limit")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errors.New("invalid JSON object member")
				}
				if seen[name] {
					return fmt.Errorf("duplicate JSON object member %q", name)
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected trailing JSON (%v)", err)
	}
	return nil
}
