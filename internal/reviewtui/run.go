package reviewtui

import (
	"errors"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

// ErrNotTerminal is returned when stdin or stdout is not a terminal.
var ErrNotTerminal = errors.New("review tui needs an interactive terminal on stdin and stdout; use `irrigator review list` and `review diff` instead")

// Run shows the review UI on the alternate screen. Bubble Tea restores the
// terminal on quit, on SIGINT/SIGTERM and when the program panics.
func Run(backend Backend) error {
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		return ErrNotTerminal
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	_, err := tea.NewProgram(newModel(backend, options{noColor: noColor})).Run()
	return err
}
