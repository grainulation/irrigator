package cli

import (
	"fmt"
	"os"
)

// Main runs the irrigator command line for args (args[0] is the program name)
// and returns the process exit code: the command's code, or 1 when the command
// failed with code 0. Errors are printed to stderr as "error: <err>".
func Main(args []string) int {
	code, err := runCommand(args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}
