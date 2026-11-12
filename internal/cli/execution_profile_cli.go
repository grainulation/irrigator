package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/provider"
)

func executionProfilesCLI(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("usage: profiles inspect|resolve --catalog ABS_FILE [--profile NAME]")
	}
	if args[0] == "migrate-prepare" || args[0] == "migrate-apply" {
		return provider.ProviderMigrationCLI(args)
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Println("profiles inspect --catalog ABS_FILE\nprofiles resolve --catalog ABS_FILE [--profile NAME]\nprofiles migrate-prepare --run RUN --profile NAME --executable FILE --out PROPOSAL\nprofiles migrate-apply --run RUN --proposal FILE --approve DIGEST --confirm-stopped\nInspect/resolve/prepare do not dispatch or change run configuration. Apply records an explicitly approved future executable binding.")
		return 0, nil
	}
	if args[0] != "inspect" && args[0] != "resolve" {
		return 2, errors.New("unknown profiles command")
	}
	fs := flag.NewFlagSet("profiles "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("catalog", "", "absolute catalog file")
	profile := fs.String("profile", "", "profile name (resolve only; default from catalog)")
	if err := commandflags.ParseCommandFlags(fs, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 0 || *path == "" {
		return 2, errors.New("profiles requires --catalog ABS_FILE and no positional arguments")
	}
	if args[0] == "inspect" {
		if *profile != "" {
			return 2, errors.New("--profile applies only to resolve")
		}
		catalog, err := launch.ReadExecutionCatalog(*path)
		if err != nil {
			return 1, err
		}
		return 0, jsonio.PrintJSON(map[string]any{"catalog": catalog, "catalog_sha256": launch.ExecutionDigest(catalog), "executable_verified": false, "auth_verified": false, "dispatched": false, "capability_evidence": "declared_contract_not_live_probe"})
	}
	binding, err := launch.ResolveExecutionProfile(*path, *profile)
	if err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(binding)
}
