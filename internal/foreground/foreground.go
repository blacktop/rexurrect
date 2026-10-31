// Package foreground ships the shell hooks a Rex pane's shell sources: the
// recorder of the command the pane is running, and the delivery handler that
// puts a saved command back after Rex restored the pane as a bare shell. An
// installed binary prints them with `rexurrect hook` without a checkout.
package foreground

import (
	"embed"
	"fmt"
)

//go:embed foreground.zsh foreground.fish deliver.zsh deliver.fish
var scripts embed.FS

// Script returns the hook for shell, which is zsh or fish: the foreground
// recorder followed by the delivery handler.
func Script(shell string) ([]byte, error) {
	if shell != "zsh" && shell != "fish" {
		return nil, fmt.Errorf("no shell hook for %q; use zsh or fish", shell)
	}
	recorder, err := scripts.ReadFile("foreground." + shell)
	if err != nil {
		return nil, err
	}
	deliver, err := scripts.ReadFile("deliver." + shell)
	if err != nil {
		return nil, err
	}
	return append(recorder, deliver...), nil
}
