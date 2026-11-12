package provider

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/grainulation/irrigator/internal/commandflags"
)

func ProviderCLI(args []string) (int, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	if args[0] == "connect" {
		if err := commandflags.ParseCommandFlags(fs, args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0, nil
			}
			return 2, err
		}
		if fs.NArg() != 1 {
			return 2, errors.New("connect expects the provider connection.json path")
		}
		if err := ConnectProvider(ctx, fs.Arg(0), os.Stdin, os.Stdout); err != nil {
			return 1, err
		}
		return 0, nil
	}
	opts := ProviderPoolOptions{}
	fs.StringVar(&opts.Dir, "dir", "", "durable service state directory (required)")
	fs.StringVar(&opts.listen, "listen", "127.0.0.1:0", "loopback listening address")
	fs.StringVar(&opts.Model, "model", "haiku", "Claude model")
	fs.StringVar(&opts.Claude, "claude", "claude", "Claude CLI executable")
	fs.IntVar(&opts.workers, "workers", 4, "maximum reusable sessions")
	fs.Float64Var(&opts.Budget, "budget", 1, "total service admission budget USD; unknown charges retain reservations")
	if err := commandflags.ParseCommandFlags(fs, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, errors.New("serve accepts flags only; use --dir")
	}
	if err := serveProvider(ctx, opts); err != nil {
		return 1, err
	}
	return 0, nil
}
