package cli

import (
	"errors"
	"flag"
	"time"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/provider"
)

func providerLimitCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("quota", flag.ContinueOnError)
	attempt := fs.Int("attempt", 0, "exact attempt number (required)")
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 2 || *attempt < 1 {
		return 2, errors.New("quota RUN NODE --attempt N (read-only provider reset advisory)")
	}
	result, err := provider.DiagnoseLimit(fs.Arg(0), fs.Arg(1), *attempt, time.Now().UTC())
	if err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(result)
}
