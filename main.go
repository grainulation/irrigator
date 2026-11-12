// Command irrigator runs durable local agent graphs. All behaviour lives in
// internal/cli; this file only forwards the process arguments and exit code.
package main

import (
	"os"

	"github.com/grainulation/irrigator/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args)) }
