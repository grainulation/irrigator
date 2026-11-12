package graph

import (
	"errors"
	"flag"
	"fmt"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

// turnShapeCLI previews the new contract without reinterpreting v1 plans.
func TurnShapeCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("turn-shape", flag.ContinueOnError)
	asJSON := false
	fs.BoolVar(&asJSON, "json", false, "JSON capacity preview")
	fs.Usage = func() {
		fmt.Println("irrigator turn-shape XxYxZ [--json]\nCapacity preview only. X independent main agents; Y optional sequential turns each; Z one-shot helpers per turn. Z=1 is unbranched shorthand (zero helpers). Existing size --shape retains legacy tree semantics.")
	}
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("expected one XxYxZ shape")
	}
	shape := fs.Arg(0)
	report, err := model.ParseTurnShape(shape)
	if err != nil {
		return 2, err
	}
	if asJSON {
		return 0, jsonio.PrintJSON(report)
	}
	fmt.Printf("Turn-based shape: %d independent main agents · up to %d sequential turns each · up to %d one-shot helpers per turn\n", report.Shape.Roots, report.Shape.MainTurns, report.Shape.HelpersPerTurn)
	fmt.Printf("Capacity: %d main executions + %d helper executions = %d executions; up to %d distinct agents. Retries excluded.\n", report.MaxMainExecutions, report.MaxHelperExecutions, report.MaxExecutions, report.MaxAgents)
	fmt.Println("Preview only: optional capacity, no execution or provider calls. No global turn barrier; helpers cannot continue or spawn.")
	return 0, nil
}
