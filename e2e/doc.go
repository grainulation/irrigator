// Package e2e holds black-box CLI contract tests: they run irrigator as a
// separate process (the test binary re-executed through cli.Main, see
// main_test.go) and compare its help, exit codes and output with the golden
// files in verification/golden. It has no production code.
package e2e
