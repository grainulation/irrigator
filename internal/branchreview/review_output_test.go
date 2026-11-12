package branchreview

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
)

func localReviewCLIOutput(t *testing.T, args []string) string {
	t.Helper()
	saved := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = saved; r.Close(); w.Close() }()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() { data, err := io.ReadAll(r); done <- result{data, err} }()
	code, runErr := ReviewCLI(args)
	w.Close()
	output := <-done
	if runErr != nil || code != 0 {
		t.Fatalf("review %v: code %d: %v", args, code, runErr)
	}
	if output.err != nil {
		t.Fatal(output.err)
	}
	return string(output.data)
}
func TestLocalReviewRequestOutputIsConciseOrExplicitJSON(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	workspace := runfixture.CanonicalContextDir(t)
	body := "Private proposal content should only appear on explicit inspection.\n"
	os.WriteFile(filepath.Join(workspace, "proposal.md"), []byte(body), 0600)
	for _, asJSON := range []bool{false, true} {
		name := "human"
		if asJSON {
			name = "json"
		}
		out := filepath.Join(workspace, name)
		args := []string{"request", "--kind", "direction", "--title", "Next step", "--workspace", workspace, "--body-file", "proposal.md", "--out", out}
		if asJSON {
			args = append(args, "--json")
		}
		output := localReviewCLIOutput(t, args)
		saved, err := readReview(out)
		if err != nil {
			t.Fatal(err)
		}
		if asJSON {
			var s reviewSnapshot
			if err = json.Unmarshal([]byte(output), &s); err != nil {
				t.Fatal(err)
			}
			if s.ID != saved.ID || s.Kind != "direction" || s.Body != body {
				t.Fatal("explicit JSON lost snapshot content")
			}
		} else {
			if !strings.Contains(output, "Direction review ready: Next step") || !strings.Contains(output, out) || strings.Contains(output, strings.TrimSpace(body)) || json.Valid([]byte(output)) {
				t.Fatalf("unexpected human output: %s", output)
			}
		}
	}
}
func TestLocalReviewCaptureOutputIsConciseOrExplicitJSON(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	repo := reviewTestRepo(t)
	private := "Private source change is not a completion summary.\n"
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte(private), 0600)
	for _, asJSON := range []bool{false, true} {
		out := reviewTestOut(t)
		args := []string{"capture", "--workspace", repo, "--out", out}
		if asJSON {
			args = append(args, "--json")
		}
		output := localReviewCLIOutput(t, args)
		saved, err := readReview(out)
		if err != nil {
			t.Fatal(err)
		}
		if asJSON {
			var s reviewSnapshot
			if err = json.Unmarshal([]byte(output), &s); err != nil {
				t.Fatal(err)
			}
			if s.ID != saved.ID || len(s.Files) != 1 || s.Files[0].After != private || len(s.Inventory) == 0 {
				t.Fatal("explicit JSON lost code snapshot content")
			}
		} else {
			if !strings.Contains(output, "Code review ready · 1 changed file") || !strings.Contains(output, out) || strings.Contains(output, strings.TrimSpace(private)) || strings.Contains(output, "inventory") || json.Valid([]byte(output)) {
				t.Fatalf("unexpected human output: %s", output)
			}
		}
	}
}
