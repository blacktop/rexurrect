// Package launchagent manages one explicitly labeled per-user launchd job.
package launchagent

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blacktop/rexurrect/internal/autolog"
)

const (
	DefaultLabel    = "io.blacktop.rexurrect"
	DefaultInterval = time.Minute
	maxPlistSize    = 64 << 10
)

type Config struct {
	Executable, Server, StateDir, Rex, Label string
	ResolvedRex                              string // Optional path already resolved by the command.
	Interval, Wait, Timeout, CallTimeout     time.Duration
	LogMaxBytes                              int64
	Scrollback                               bool // Pass --scrollback to each automatic cycle.
}

func validLabel(label string) bool {
	if len(label) == 0 || len(label) > 128 || label == "." || label == ".." {
		return false
	}
	for _, c := range label {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		digit := c >= '0' && c <= '9'
		if letter || digit || c == '.' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func (c Config) validate() error {
	if !validLabel(c.Label) {
		return errors.New("invalid LaunchAgent label")
	}
	for _, path := range []string{c.Executable, c.StateDir, c.Rex} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") || len(path) > 4096 {
			return errors.New("LaunchAgent executable, state directory, and Rex paths must be absolute")
		}
	}
	socket, ok := strings.CutPrefix(c.Server, "unix://")
	if !ok || !filepath.IsAbs(socket) || len(socket) >= 104 || strings.ContainsAny(socket, "\x00\r\n") {
		return errors.New("LaunchAgent server must be an absolute unix:// socket path shorter than 104 bytes")
	}
	if c.Interval < time.Minute || c.Interval > 24*time.Hour || c.Interval%time.Second != 0 {
		return errors.New("--interval must be a whole number of seconds between 1m and 24h")
	}
	if c.Wait <= 0 || c.Timeout <= 0 || c.Wait > c.Timeout || c.Timeout > time.Hour {
		return errors.New("LaunchAgent requires 0 < --wait <= --timeout <= 1h")
	}
	if c.CallTimeout <= 0 || c.CallTimeout > time.Minute {
		return errors.New("--call-timeout must be positive and at most 60s")
	}
	return autolog.ValidateLimit(c.LogMaxBytes)
}

type plistValue struct {
	XMLName xml.Name
	Text    string   `xml:",chardata"`
	Strings []string `xml:"string"`
}

func encodePlistValue(enc *xml.Encoder, out *bytes.Buffer, value plistValue) error {
	if kind := value.XMLName.Local; kind == "true" || kind == "false" {
		// launchd requires the self-closing plist boolean spelling, even though
		// plutil accepts the equivalent paired XML tags emitted by encoding/xml.
		if err := enc.Flush(); err != nil {
			return err
		}
		_, err := out.WriteString("<" + kind + "/>")
		return err
	}
	return enc.Encode(value)
}

// Render uses XML elements for every argument; no shell expands their values.
func Render(c Config) ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(xml.Header + "<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n")
	enc := xml.NewEncoder(&b)
	entry := func(key, kind, value string, items []string) error {
		return errors.Join(enc.EncodeElement(key, xml.StartElement{Name: xml.Name{Local: "key"}}), encodePlistValue(enc, &b, plistValue{XMLName: xml.Name{Local: kind}, Text: value, Strings: items}))
	}
	args := []string{c.Executable, "auto", "--server", c.Server, "--state-dir", c.StateDir, "--rex", c.Rex, "--wait", c.Wait.String(), "--timeout", c.Timeout.String(), "--call-timeout", c.CallTimeout.String(), "--log-max-bytes", strconv.FormatInt(c.LogMaxBytes, 10)}
	if c.Scrollback {
		args = append(args, "--scrollback")
	}
	for _, e := range []struct{ key, kind, value string }{
		{"Label", "string", c.Label},
		{"RexResurrectManaged", "string", "1"},
		{"StartInterval", "integer", strconv.FormatInt(int64(c.Interval/time.Second), 10)},
		{"RunAtLoad", "true", ""},
		{"ProcessType", "string", "Background"},
		{"ExitTimeOut", "integer", "5"},
		{"Umask", "integer", "63"},
		{"StandardOutPath", "string", filepath.Join(c.StateDir, autolog.OutName)},
		{"StandardErrorPath", "string", filepath.Join(c.StateDir, autolog.ErrName)},
	} {
		if err := entry(e.key, e.kind, e.value, nil); err != nil {
			return nil, err
		}
	}
	if err := entry("ProgramArguments", "array", "", args); err != nil {
		return nil, err
	}
	if err := entry("WatchPaths", "array", "", []string{strings.TrimPrefix(c.Server, "unix://")}); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	b.WriteString("\n</dict></plist>\n")
	if b.Len() > maxPlistSize {
		return nil, errors.New("LaunchAgent plist exceeds size limit")
	}
	return b.Bytes(), nil
}

func parsePlist(data []byte) (map[string]plistValue, error) {
	var document struct {
		XMLName xml.Name `xml:"plist"`
		Dict    struct {
			Items []plistValue `xml:",any"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	items := document.Dict.Items
	if len(items)%2 != 0 {
		return nil, errors.New("invalid plist dictionary")
	}
	values := make(map[string]plistValue)
	for i := 0; i < len(items); i += 2 {
		key := items[i]
		if _, exists := values[key.Text]; key.XMLName.Local != "key" || exists {
			return nil, fmt.Errorf("invalid or repeated plist key %q", key.Text)
		}
		values[key.Text] = items[i+1]
	}
	return values, nil
}
