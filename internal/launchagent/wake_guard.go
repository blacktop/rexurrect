package launchagent

import (
	"context"
	"strings"
)

// launchctl's print output is diagnostic text, not a stable API. Recognize
// only the shape verified by the native tests; unfamiliar output suppresses
// this optional wake rather than dispatching a job we cannot identify.
func (a *Agent) wakeGuard(ctx context.Context, c Config, args []string, disabled plistValue) (string, error) {
	if disabled.XMLName.Local != "" && disabled.XMLName.Local != "false" {
		return "the owned LaunchAgent plist is disabled or has an invalid Disabled value", nil
	}
	job, err := a.launchctl(ctx, "print", a.target())
	if absentService(err) {
		return "the LaunchAgent is not loaded", nil
	}
	if err != nil {
		return "", err
	}
	if !loadedJobMatches(string(job), a.target(), a.PlistPath, c.Executable, args) {
		return "the loaded LaunchAgent does not match the owned plist", nil
	}
	overrides, err := a.launchctl(ctx, "print-disabled", a.Domain)
	if err != nil {
		return "", err
	}
	enabled, known := jobEnabled(string(overrides), a.Label)
	if !known {
		return "cannot determine the LaunchAgent disabled state", nil
	}
	if !enabled {
		return "the LaunchAgent is disabled", nil
	}
	return "", nil
}

func loadedJobMatches(output, target, path, executable string, args []string) bool {
	for _, value := range append([]string{target, path, executable}, args...) {
		if strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	if !strings.HasPrefix(output, target+" = {\n") ||
		strings.Count(output, "\n\tpath = ") != 1 ||
		strings.Count(output, "\n\tprogram = ") != 1 ||
		strings.Count(output, "\n\targuments = {\n") != 1 {
		return false
	}
	arguments := "\n\targuments = {\n\t\t" + strings.Join(args, "\n\t\t") + "\n\t}\n"
	return strings.Contains(output, "\n\tpath = "+path+"\n") &&
		strings.Contains(output, "\n\tprogram = "+executable+"\n") &&
		strings.Contains(output, arguments)
}

func jobEnabled(output, label string) (enabled, known bool) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "disabled services = {" || strings.TrimSpace(lines[len(lines)-1]) != "}" {
		return false, false
	}
	enabled = true // Our generated plist has no Disabled=true default.
	found := false
	for _, line := range lines[1 : len(lines)-1] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), " => ")
		if !ok || len(key) < 2 || key[0] != '"' || key[len(key)-1] != '"' {
			return false, false
		}
		if key != `"`+label+`"` {
			continue
		}
		if found {
			return false, false
		}
		found = true
		switch value {
		case "enabled", "false":
			enabled = true
		case "disabled", "true":
			enabled = false
		default:
			return false, false
		}
	}
	return enabled, true
}
