package cli

import (
	"errors"
	"flag"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/gitcontract"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
)

func runtimeContractsCLI(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("usage: irrigator contracts plan FILE | receipt RUN | git quarantine|prepare|accept|settle|reconcile ...")
	}
	fs := flag.NewFlagSet("contracts "+args[0], flag.ContinueOnError)
	if err := commandflags.ParseCommandFlags(fs, args[1:]); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, errors.New("usage: irrigator contracts plan FILE | receipt RUN | git quarantine|prepare|accept|settle|reconcile ...")
	}
	switch args[0] {
	case "plan":
		p, err := checkpoint.LoadPlan(fs.Arg(0))
		if err != nil {
			return 1, err
		}
		return 0, jsonio.PrintJSON(map[string]any{"schema": launch.RuntimeGuardrailsSchema, "basis": "implementation_declarations_not_live_attestation", "contracts": launch.RuntimeContracts(p), "requirements": p.Guardrails})
	case "git":
		return gitcontract.GitContractsCLI(args[1:])
	case "receipt":
		s, err := checkpoint.ReadState(fs.Arg(0))
		if err != nil {
			return 1, err
		}
		r, err := launch.BuildRuntimeEvidence(s)
		if err != nil {
			return 1, err
		}
		return 0, jsonio.PrintJSON(r)
	default:
		return 2, errors.New("unknown contracts subcommand")
	}
}
