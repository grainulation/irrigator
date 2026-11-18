package cli

import (
	"errors"
	"flag"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/delivery"
	"github.com/grainulation/irrigator/internal/jsonio"
)

func deliverySummaryCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("delivery-summary", flag.ContinueOnError)
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, errors.New("delivery-summary RUN (read-only candidate and check report)")
	}
	summary, err := delivery.Summarize(fs.Arg(0))
	if err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(summary)
}
