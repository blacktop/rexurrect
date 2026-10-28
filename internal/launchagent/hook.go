package launchagent

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// LuaHook renders the init.lua snippet that starts `agent wake` detached each
// time Rex loads its configuration. Every argument is a Lua string literal;
// no shell expands them. A timeout option is deliberately absent: Rex kills a
// detached command when one expires, and wake bounds its own wait.
func LuaHook(executable, label, plistDir string) ([]byte, error) {
	if !filepath.IsAbs(executable) {
		return nil, errors.New("the rexurrect executable path must be absolute")
	}
	if !validLabel(label) {
		return nil, errors.New("invalid LaunchAgent label")
	}
	if plistDir != "" && !filepath.IsAbs(plistDir) {
		return nil, errors.New("--plist-dir must be absolute")
	}
	argv := []string{executable, "agent", "wake"}
	if label != DefaultLabel {
		argv = append(argv, "--label", label)
	}
	if plistDir != "" {
		argv = append(argv, "--plist-dir", plistDir)
	}
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		literal, err := luaString(arg)
		if err != nil {
			return nil, err
		}
		quoted = append(quoted, literal)
	}
	return fmt.Appendf(nil, "-- rexurrect: wake the recovery LaunchAgent as soon as this Rex server starts.\n-- Printed by `rexurrect hook lua`; harmless under `rex config check` and reload.\nif rex.exec and rex.exec.start then\n  rex.exec.start({%s}, {detach = true})\nend\n", strings.Join(quoted, ", ")), nil
}

func luaString(s string) (string, error) {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c < 0x20 || c == 0x7f:
			return "", fmt.Errorf("argument %q contains a control character", s)
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}
