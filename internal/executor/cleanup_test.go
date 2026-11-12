package executor

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// cleanupFakes puts fake `hc` and `tmux` scripts alone on PATH. Both append
// their argv to one log and use only shell builtins. hc ls prints
// FAKE_HC_LS (a file); tmux has-session succeeds when FAKE_TMUX_SESSION (a
// file) exists and list-panes prints it. Neither talks to a real server.
func cleanupFakes(t *testing.T, withHC, withTmux bool, hcListing, tmuxPanes string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	t.Setenv("PATH", dir)
	t.Setenv("FAKE_LOG", log)
	cat := `while IFS= read -r l; do printf '%s\n' "$l"; done <`
	if withHC {
		cleanupWrite(t, filepath.Join(dir, "listing.json"), hcListing+"\n")
		script := "#!/bin/sh\necho \"hc $*\" >> \"$FAKE_LOG\"\ncase \"$1\" in\n" +
			"--version) echo 'hc 0.1.19' ;;\n" +
			"ls) " + cat + " " + filepath.Join(dir, "listing.json") + " ;;\n" +
			"rm) exit 0 ;;\nesac\n"
		cleanupWrite(t, filepath.Join(dir, "hc"), script)
	}
	if withTmux {
		cleanupWrite(t, filepath.Join(dir, "panes.txt"), tmuxPanes)
		script := "#!/bin/sh\necho \"tmux $*\" >> \"$FAKE_LOG\"\ncase \"$1\" in\n" +
			"-V) echo 'tmux 3.4' ;;\n" +
			"has-session) [ -s " + filepath.Join(dir, "panes.txt") + " ] ;;\n" +
			"list-panes) " + cat + " " + filepath.Join(dir, "panes.txt") + " ;;\n" +
			"kill-session) exit 0 ;;\nesac\n"
		cleanupWrite(t, filepath.Join(dir, "tmux"), script)
	}
	return log
}

func cleanupWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func cleanupCalls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func cleanupHas(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestCleanupHerdrRemovesOnlyFinishedTerminals(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01))
	runID := runfixture.Read(t, root).RunID
	log := cleanupFakes(t, true, false, `[{"name":"irr-1-a-1","state":"finished"},{"name":"irr-1-b-1","state":"running"},{"name":"irr-1-c-1"},{"name":"irr-1-d-1","state":"exited"}]`, "")
	r, err := cleanupRun(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Herdr.Removed, ","); got != "irr-1-a-1,irr-1-d-1" {
		t.Fatalf("removed %q", got)
	}
	if len(r.Herdr.Kept) != 2 || r.Herdr.Status != "cleaned" {
		t.Fatalf("kept %v status %s", r.Herdr.Kept, r.Herdr.Status)
	}
	calls := cleanupCalls(t, log)
	want := "hc ls --all --json --label irrigator.run=" + runID
	if !cleanupHas(calls, want) {
		t.Fatalf("missing %q in %v", want, calls)
	}
	rm := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "hc rm ") {
			rm++
			if strings.Contains(c, "-b-") || strings.Contains(c, "-c-") {
				t.Fatalf("removed a live terminal: %s", c)
			}
		}
	}
	if rm != 2 {
		t.Fatalf("hc rm calls: %v", calls)
	}
	if r.Tmux.Status != "skipped" {
		t.Fatalf("tmux not installed should be skipped, got %s", r.Tmux.Status)
	}
}

func TestCleanupDryRunChangesNothing(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01))
	before, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	log := cleanupFakes(t, true, true, `[{"name":"irr-1-a-1","state":"finished"}]`, "a-a1 1\n")
	r, err := cleanupRun(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Herdr.Status != "would_clean" || r.Tmux.Status != "would_clean" {
		t.Errorf("dry-run statuses %q %q, want would_clean", r.Herdr.Status, r.Tmux.Status)
	}
	if !r.DryRun || len(r.Herdr.Removed) != 1 || len(r.Tmux.Removed) != 1 {
		t.Fatalf("dry run should list both: %+v", r)
	}
	calls := cleanupCalls(t, log)
	if cleanupHas(calls, "hc rm") || cleanupHas(calls, "tmux kill-session") {
		t.Fatalf("dry run mutated: %v", calls)
	}
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if string(before) != string(after) {
		t.Fatal("run state changed")
	}
}

func TestCleanupTmuxKillsSessionOnlyWhenAllPanesDead(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01))
	runID := runfixture.Read(t, root).RunID
	session := TmuxSessionName(runID)

	log := cleanupFakes(t, false, true, "", "a-a1 1\nb-a1 1\n")
	r, err := cleanupRun(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tmux.Removed) != 1 || r.Tmux.Removed[0] != session || r.Herdr.Status != "skipped" {
		t.Fatalf("dead session: %+v", r)
	}
	if !cleanupHas(cleanupCalls(t, log), "tmux kill-session -t ="+session) {
		t.Fatalf("no kill-session: %v", cleanupCalls(t, log))
	}

	log = cleanupFakes(t, false, true, "", "a-a1 1\nb-a1 0\n")
	r, err = cleanupRun(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tmux.Removed) != 0 || len(r.Tmux.Kept) != 1 || !strings.Contains(r.Tmux.Kept[0], "b-a1") {
		t.Fatalf("live window should be listed and kept: %+v", r.Tmux)
	}
	if cleanupHas(cleanupCalls(t, log), "tmux kill-session") {
		t.Fatal("killed a session with a live pane")
	}
}

func TestCleanupTmuxMissingSessionIsNothing(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01))
	cleanupFakes(t, false, true, "", "")
	r, err := cleanupRun(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tmux.Status != "nothing" || len(r.Tmux.Removed) != 0 {
		t.Fatalf("%+v", r.Tmux)
	}
}

func TestCleanupSkipsToolsNotInstalled(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01))
	cleanupFakes(t, false, false, "", "")
	r, err := cleanupRun(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Herdr.Status != "skipped" || r.Tmux.Status != "skipped" {
		t.Fatalf("%+v", r)
	}
}

func TestCleanupHerdrListingErrorIsReported(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01))
	cleanupFakes(t, true, false, `not json`, "")
	r, err := cleanupRun(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Herdr.Status != "error" || len(r.Herdr.Removed) != 0 {
		t.Fatalf("%+v", r.Herdr)
	}
}

func TestCleanupCLIFlagsAfterRunAndAllFinished(t *testing.T) {
	parent := t.TempDir()
	var ids []string
	for _, name := range []string{"one", "two"} {
		root := filepath.Join(parent, name)
		if err := checkpoint.Initialize(model.Plan{Name: name, Tasks: []model.Task{runfixture.Task("a", .01)}}, root, "mock"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, runfixture.Read(t, root).RunID)
	}
	if err := os.Mkdir(filepath.Join(parent, "not-a-run"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := cleanupFakes(t, true, false, `[{"name":"t1","state":"finished"}]`, "")

	stdout := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wr
	code, cerr := CleanupCLI([]string{parent, "--all-finished", "--dry-run"})
	os.Stdout = stdout
	wr.Close()
	out, _ := io.ReadAll(rd)
	if code != 0 || cerr != nil {
		t.Fatalf("cleanup: %d %v", code, cerr)
	}
	var reports []cleanupReport
	if err := json.Unmarshal(out, &reports); err != nil || len(reports) != 2 {
		t.Fatalf("reports %v %s", err, out)
	}
	for _, id := range ids {
		if !cleanupHas(cleanupCalls(t, log), "hc ls --all --json --label irrigator.run="+id) {
			t.Fatalf("run %s not listed", id)
		}
	}
	if cleanupHas(cleanupCalls(t, log), "hc rm") {
		t.Fatal("dry run removed")
	}
	if code, err := CleanupCLI(nil); code != 2 || err == nil {
		t.Fatalf("missing RUN: %d %v", code, err)
	}
}
