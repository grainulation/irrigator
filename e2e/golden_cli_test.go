package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/cli"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestGoldenHelpMatchesRuntime(t *testing.T) {
	goldenPath := filepath.Join(runfixture.GoldenDir(), "help.golden")
	wantBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden help: %v", err)
	}
	want := strings.TrimSpace(string(wantBytes))
	got := strings.TrimSpace(cli.Usage)
	if got != want {
		t.Errorf("usage does not match %s:\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
	}
}

func TestGoldenExitCodes(t *testing.T) {
	goldenQuietEnv(t)
	goldenPath := filepath.Join(runfixture.GoldenDir(), "exit_codes.json")
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading exit_codes.json: %v", err)
	}
	var cases []struct {
		Name         string   `json:"name"`
		Args         []string `json:"args"`
		ExpectedExit int      `json:"exit_code"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("unmarshaling exit codes: %v", err)
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			code, _ := goldenExecute(t, c.Args)
			if code != c.ExpectedExit {
				t.Errorf("cli(%v) exit code = %d, want %d", c.Args, code, c.ExpectedExit)
			}
		})
	}
}

// TestGoldenCLIProcess executes the production entrypoint in an isolated copy
// of the test process. Running main verifies real exit codes and error output;
// the parent never swaps global stdout/stderr or manufactures an error line.
func TestGoldenCLIProcess(t *testing.T) {
	if os.Getenv("IRRIGATOR_GOLDEN_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"irrigator"}, os.Args[i+1:]...)
			os.Exit(cli.Main(os.Args))
			return
		}
	}
	os.Exit(97)
}

func goldenExecute(t *testing.T, args []string) (int, string) {
	t.Helper()
	argv := append([]string{"-test.run=^TestGoldenCLIProcess$", "--"}, args...)
	cmd := exec.Command(os.Args[0], argv...)
	cmd.Env = append(os.Environ(), "IRRIGATOR_GOLDEN_PROCESS=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	}
	t.Fatalf("cannot launch CLI fixture process: %v", err)
	return 0, ""
}

func goldenRun(t *testing.T, args []string) string {
	t.Helper()
	_, out := goldenExecute(t, args)
	return out
}

// goldenQuietEnv removes ambient configuration that could change flag defaults.
func goldenQuietEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "IRRIGATOR_") {
			t.Setenv(name, "")
		}
	}
}

// TestGoldenSubcommandHelpMatchesRuntime runs every committed fixture's command
// with -h through the real dispatcher. -h makes each command's own flag parser
// print its usage and return before any run, provider, or file effect.
func TestGoldenSubcommandHelpMatchesRuntime(t *testing.T) {
	goldenQuietEnv(t)
	for _, name := range runfixture.GoldenFixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(runfixture.GoldenDir(), "subcommands", name+".golden")
			wantBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got := goldenRun(t, []string{name, "-h"})
			if got != string(wantBytes) {
				t.Errorf("`irrigator %s -h` does not match %s (regenerate only after reviewing a real contract change):\n--- got ---\n%s\n--- want ---\n%s", name, path, got, wantBytes)
			}
		})
	}
}

func TestGoldenHelpEntryPointsPrintUsage(t *testing.T) {
	wantBytes, err := os.ReadFile(filepath.Join(runfixture.GoldenDir(), "help.golden"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(string(wantBytes))
	for _, args := range [][]string{nil, {"-h"}, {"--help"}, {"help"}} {
		if got := strings.TrimSpace(goldenRun(t, args)); got != want {
			t.Errorf("cli(%q) output does not match help.golden:\n--- got ---\n%s", args, got)
		}
	}
}
