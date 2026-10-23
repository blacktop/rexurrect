// Package foreground ships the shell hooks that record the command a Rex pane
// is running, so an installed binary can print them with `rexurrect hook`
// without a checkout of the repository.
package foreground

import (
	"embed"
	"fmt"
)

//go:embed foreground.zsh foreground.fish
var scripts embed.FS

// Script returns the recorder hook for shell, which is zsh or fish.
func Script(shell string) ([]byte, error) {
	switch shell {
	case "zsh", "fish":
		return scripts.ReadFile("foreground." + shell)
	default:
		return nil, fmt.Errorf("no foreground hook for shell %q; use zsh or fish", shell)
	}
}
