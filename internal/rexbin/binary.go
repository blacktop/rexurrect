// Package rexbin finds the Rex CLI without starting or contacting a server.
package rexbin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tests set this with -ldflags so compiled commands search a private fixture.
var applicationsDir = "/Applications"

type Substitution struct {
	Configured string `json:"configured"`
	Resolved   string `json:"resolved"`
}

// Resolve prefers the configured executable, then the Beta and stable app
// helpers in lexical order, then rex on PATH. Returned paths are absolute.
func Resolve(configured string) (string, *Substitution, error) {
	if configured == "" {
		return "", nil, errors.New("configured Rex executable path must be nonempty")
	}
	lookup := func(name string) (string, error) {
		path, err := exec.LookPath(name)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular executable file", path)
		}
		return filepath.Abs(path)
	}
	if path, err := lookup(configured); err == nil {
		return path, nil, nil
	}
	searched := []string{configured}
	for _, app := range []string{"Rex Beta.app", "Rex.app"} {
		pattern := filepath.Join(applicationsDir, app, "Contents", "Helpers", "*", "Contents", "MacOS", "rex")
		searched = append(searched, pattern)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", nil, fmt.Errorf("search Rex app helpers %q: %w", pattern, err)
		}
		for _, candidate := range matches {
			if path, err := lookup(candidate); err == nil {
				return path, &Substitution{Configured: configured, Resolved: path}, nil
			}
		}
	}
	if path, err := lookup("rex"); err == nil {
		return path, &Substitution{Configured: configured, Resolved: path}, nil
	}
	searched = append(searched, "PATH="+os.Getenv("PATH"))
	return "", nil, fmt.Errorf("no executable Rex CLI found; searched %s", strings.Join(searched, "; "))
}
