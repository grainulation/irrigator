package commandflags

import (
	"flag"
	"testing"
)

func TestCommandArgumentParsingUsesRegisteredFlags(t *testing.T) {
	for _, args := range [][]string{{"plan.json", "--out=result.json", "--json=false"}, {"--out", "result.json", "plan.json", "--json=false"}} {
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		output := fs.String("out", "", "output")
		jsonOutput := fs.Bool("json", true, "JSON")
		if err := ParseCommandArguments(fs, args); err != nil {
			t.Fatal(err)
		}
		if *output != "result.json" || *jsonOutput || fs.NArg() != 1 || fs.Arg(0) != "plan.json" {
			t.Fatalf("incorrect parsed arguments: %v", args)
		}
	}
}
