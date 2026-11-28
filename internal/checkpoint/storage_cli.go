package checkpoint

import (
	"errors"
	"flag"
	"path/filepath"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
)

// StorageCLI explicitly migrates a saved run; no provider work is started.
func StorageCLI(args []string) (int, error) {
	if len(args) == 0 || args[0] != "migrate" {
		return 2, errors.New("usage: storage migrate --run DIR --format journal")
	}
	fs := flag.NewFlagSet("storage migrate", flag.ContinueOnError)
	root := fs.String("run", "", "saved run directory")
	format := fs.String("format", "", "required: journal")
	if err := commandflags.ParseCommandFlags(fs, args[1:]); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 || *root == "" || *format != "journal" {
		return 2, errors.New("storage migrate requires --run and --format journal")
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		return 2, err
	}
	if err = EnableJournal(abs); err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(map[string]any{"run": abs, "storage": "irrigator.journal/v1", "dispatched": false, "original_backup": "state.json.bak", "older_clients": "read/mutation unsupported; use original backup only as historical evidence"})
}
