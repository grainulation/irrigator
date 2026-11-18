package branchreview

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/session"
)

func applyTestHead(t *testing.T, ws, branch string) string {
	t.Helper()
	head, err := changeRev(ws, branch)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func applyTest(t *testing.T, ws string, files ...string) *appliedChange {
	t.Helper()
	state, err := applySelectedChange(ws, "agent-1", "main", applyTestHead(t, ws, "agent-1"), files, []string{"/usr/bin/true"}, 10*time.Second)
	if err != nil {
		t.Fatalf("apply: %v; state: %+v", err, state)
	}
	return state
}

func TestSelectiveApplyVerifiesOnlySelectedCandidateAndUndoPreservesUnrelatedDirtyIndex(t *testing.T) {
	ws := changeTestRepo(t)
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "add", "go.mod")
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("keep untracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := session.ReviewGit(ws, "ls-files", "--stage", "-z")
	if err != nil {
		t.Fatal(err)
	}
	state, err := applySelectedChange(ws, "agent-1", "main", applyTestHead(t, ws, "agent-1"), []string{"app.go"},
		[]string{"/bin/sh", "-c", "grep -q '^B$' app.go && test ! -e db/migrations/1.sql"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "verified" || state.Verification.Status != "passed" || state.Verification.ExitCode == nil || *state.Verification.ExitCode != 0 || state.Verification.CandidateSHA256 == "" {
		t.Fatalf("missing exact-candidate verification: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(ws, "db/migrations/1.sql")); !os.IsNotExist(err) {
		t.Fatal("unselected migration applied")
	}
	if _, err := undoSelectedChange(ws, state.ID); err != nil {
		t.Fatal(err)
	}
	after, err := session.ReviewGit(ws, "ls-files", "--stage", "-z")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("index changed after undo: %v\nbefore %q\nafter %q", err, before, after)
	}
	for file, want := range map[string]string{"app.go": "a\nb\nc\n", "go.mod": "module dirty\n", "notes.txt": "keep untracked\n"} {
		got, err := os.ReadFile(filepath.Join(ws, file))
		if err != nil || string(got) != want {
			t.Fatalf("%s lost: %q, %v", file, got, err)
		}
	}
}

func TestSelectiveApplyRequiresReviewedHeadFilesAndVerifier(t *testing.T) {
	ws := changeTestRepo(t)
	head := applyTestHead(t, ws, "agent-1")
	for _, tc := range []struct {
		head   string
		files  []string
		verify []string
	}{
		{"", []string{"app.go"}, []string{"/usr/bin/true"}},
		{applyTestHead(t, ws, "main"), []string{"app.go"}, []string{"/usr/bin/true"}},
		{head, nil, []string{"/usr/bin/true"}},
		{head, []string{"app.go"}, nil},
		{head, []string{"../outside"}, []string{"/usr/bin/true"}},
		{head, []string{".irrigator/changes/x"}, []string{"/usr/bin/true"}},
	} {
		if state, err := applySelectedChange(ws, "agent-1", "main", tc.head, tc.files, tc.verify, time.Second); err == nil || state != nil {
			t.Fatalf("invalid apply admitted: %+v; state %+v err %v", tc, state, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(ws, "app.go")); err != nil || string(data) != "a\nb\nc\n" {
		t.Fatal("invalid invocation modified workspace")
	}
}

func TestSelectiveApplyRefusesDirtySelectedFileAndConflict(t *testing.T) {
	ws := changeTestRepo(t)
	if err := os.WriteFile(filepath.Join(ws, "app.go"), []byte("user work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if state, err := applySelectedChange(ws, "agent-1", "main", applyTestHead(t, ws, "agent-1"), []string{"app.go"}, []string{"/usr/bin/true"}, time.Second); err == nil || state != nil {
		t.Fatalf("dirty selected content overwritten: %+v %v", state, err)
	}
	reviewTestGit(t, ws, "add", "app.go")
	reviewTestGit(t, ws, "commit", "-qm", "conflicting target")
	if state, err := applySelectedChange(ws, "agent-1", "main", applyTestHead(t, ws, "agent-1"), []string{"app.go"}, []string{"/usr/bin/true"}, time.Second); err == nil || state != nil {
		t.Fatalf("conflicting candidate applied: %+v %v", state, err)
	}
}

func TestSelectiveUndoRefusesWorktreeIndexModeAndHeadDrift(t *testing.T) {
	for _, drift := range []string{"worktree", "index", "mode", "head"} {
		t.Run(drift, func(t *testing.T) {
			ws := changeTestRepo(t)
			state := applyTest(t, ws, "app.go")
			switch drift {
			case "worktree":
				if err := os.WriteFile(filepath.Join(ws, "app.go"), []byte("later edit\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "index":
				reviewTestGit(t, ws, "reset", "-q", "HEAD", "--", "app.go")
			case "mode":
				if err := os.Chmod(filepath.Join(ws, "app.go"), 0700); err != nil {
					t.Fatal(err)
				}
			case "head":
				reviewTestGit(t, ws, "commit", "-qm", "candidate committed")
			}
			if _, err := undoSelectedChange(ws, state.ID); err == nil {
				t.Fatal("undo overwrote post-apply work")
			}
		})
	}
}

func TestSelectiveApplyFailedAndMutatingVerifierNeverEarnGreen(t *testing.T) {
	for _, argv := range [][]string{{"/usr/bin/false"}, {"/bin/sh", "-c", "printf 'verifier changed source\n' > app.go"}} {
		ws := changeTestRepo(t)
		state, err := applySelectedChange(ws, "agent-1", "main", applyTestHead(t, ws, "agent-1"), []string{"app.go"}, argv, 10*time.Second)
		if err == nil || state == nil || state.Status != "verification_failed" || state.Verification.Status == "passed" {
			t.Fatalf("invalid green result: %+v, %v", state, err)
		}
		saved, readErr := readApplied(ws, state.ID)
		if readErr != nil || saved.Status != "verification_failed" {
			t.Fatalf("failure receipt missing: %+v, %v", saved, readErr)
		}
		if argv[0] == "/usr/bin/false" {
			if state.Verification.ExitCode == nil || *state.Verification.ExitCode != 1 {
				t.Fatalf("actual failed exit status not observed: %+v", state.Verification)
			}
			if _, err := undoSelectedChange(ws, state.ID); err != nil {
				t.Fatal(err)
			}
		} else if _, err := undoSelectedChange(ws, state.ID); err == nil {
			t.Fatal("undo overwrote verifier source mutation")
		}
	}
}

func TestSelectiveApplyRejectsHiddenIndexFlags(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		ws := changeTestRepo(t)
		reviewTestGit(t, ws, "update-index", flag, "app.go")
		if _, err := applySelectedChange(ws, "agent-1", "main", applyTestHead(t, ws, "agent-1"), []string{"app.go"}, []string{"/usr/bin/true"}, time.Second); err == nil {
			t.Fatalf("selected path with %s flag admitted", flag)
		}
	}
}

func TestSelectiveUndoRequiresIntactJournalAndConfirmedProcessCleanup(t *testing.T) {
	for _, mutation := range []string{"digest", "index_path", "active_process", "missing_process"} {
		t.Run(mutation, func(t *testing.T) {
			ws := changeTestRepo(t)
			state := applyTest(t, ws, "app.go")
			dir := filepath.Join(ws, ".irrigator/applied")
			switch mutation {
			case "digest":
				state.Before[0].Data = []byte("tampered")
				data, _ := json.Marshal(state)
				if err := os.WriteFile(filepath.Join(dir, state.ID+".json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "index_path":
				state.Before[0].Index = bytes.ReplaceAll(state.Before[0].Index, []byte("app.go"), []byte("other.go"))
				if err := saveApplied(dir, state); err != nil {
					t.Fatal(err)
				}
			case "active_process":
				if err := os.WriteFile(filepath.Join(dir, state.ID+"-verify.process.json"), []byte(`{"status":"active"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing_process":
				if err := os.Remove(filepath.Join(dir, state.ID+"-verify.process.json")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := undoSelectedChange(ws, state.ID); err == nil {
				t.Fatal("unsafe undo admitted")
			}
			if got, err := os.ReadFile(filepath.Join(ws, "app.go")); err != nil || string(got) != "a\nB\nc\n" {
				t.Fatal("failed undo changed source")
			}
		})
	}
}

func TestSelectiveVerificationTimeoutRecordsFailureAndAllowsOnlyExplicitUndo(t *testing.T) {
	ws := changeTestRepo(t)
	state, err := applySelectedChange(ws, "agent-1", "main", applyTestHead(t, ws, "agent-1"), []string{"app.go"}, []string{"/bin/sh", "-c", "sleep 5"}, 100*time.Millisecond)
	if err == nil || state == nil || state.Status != "verification_failed" || state.Verification.Status != "timed_out" {
		t.Fatalf("timeout not explicit: %+v, %v", state, err)
	}
	if _, err := undoSelectedChange(ws, state.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSelectiveApplyHandlesBinaryRenameExecutableAndSymlinkWithoutFollowing(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "switch", "-qc", "rich")
	reviewTestGit(t, ws, "mv", "app.go", "renamed.go")
	binary := []byte{0, 255, 1, 2, 0, 3}
	if err := os.WriteFile(filepath.Join(ws, "binary.dat"), binary, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "script.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, []byte("outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "add", ".")
	reviewTestGit(t, ws, "commit", "-qm", "rich candidate")
	reviewTestGit(t, ws, "switch", "-q", "main")
	state, err := applySelectedChange(ws, "rich", "main", applyTestHead(t, ws, "rich"), []string{"renamed.go", "binary.dat", "script.sh", "link"}, []string{"/usr/bin/true"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Paths) != 5 {
		t.Fatalf("rename source not selected with destination: %v", state.Paths)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "binary.dat")); err != nil || !bytes.Equal(got, binary) {
		t.Fatal("binary changed")
	}
	if info, err := os.Stat(filepath.Join(ws, "script.sh")); err != nil || info.Mode()&0100 == 0 {
		t.Fatal("executable mode lost")
	}
	if got, err := os.Readlink(filepath.Join(ws, "link")); err != nil || got != outside {
		t.Fatal("link text changed")
	}
	if _, err := undoSelectedChange(ws, state.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside\n" {
		t.Fatal("symlink target was followed")
	}
	if _, err := os.Lstat(filepath.Join(ws, "link")); !os.IsNotExist(err) {
		t.Fatal("added link not removed")
	}
	if got, err := os.ReadFile(filepath.Join(ws, "app.go")); err != nil || string(got) != "a\nb\nc\n" {
		t.Fatal("rename not restored")
	}
}

func TestSelectiveApplyRejectsSymlinkAncestorsAndLiteralPathspecs(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "switch", "-qc", "literal")
	for file, body := range map[string]string{"a[1].txt": "literal\n", "dir/file": "new\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, file)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reviewTestGit(t, ws, "add", ".")
	reviewTestGit(t, ws, "commit", "-qm", "literal candidate")
	reviewTestGit(t, ws, "switch", "-q", "main")
	if err := os.Symlink(t.TempDir(), filepath.Join(ws, "dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := applySelectedChange(ws, "literal", "main", applyTestHead(t, ws, "literal"), []string{"dir/file"}, []string{"/usr/bin/true"}, time.Second); err == nil {
		t.Fatal("symlink ancestor admitted")
	}
	state, err := applySelectedChange(ws, "literal", "main", applyTestHead(t, ws, "literal"), []string{"a[1].txt"}, []string{"/usr/bin/true"}, 10*time.Second)
	if err != nil || state == nil || state.Status != "verified" {
		t.Fatalf("literal path not supported: %+v, %v", state, err)
	}
}

func TestSelectiveApplyRestoresDeletedFileAndChangedSymlink(t *testing.T) {
	ws := changeTestRepo(t)
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("outside preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "existing-link")); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "add", "existing-link")
	reviewTestGit(t, ws, "commit", "-qm", "base link")
	reviewTestGit(t, ws, "switch", "-qc", "delete-and-link")
	reviewTestGit(t, ws, "rm", "app.go", "existing-link")
	if err := os.Symlink("other-target", filepath.Join(ws, "existing-link")); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "add", "existing-link")
	reviewTestGit(t, ws, "commit", "-qm", "delete and link")
	reviewTestGit(t, ws, "switch", "-q", "main")
	state, err := applySelectedChange(ws, "delete-and-link", "main", applyTestHead(t, ws, "delete-and-link"), []string{"app.go", "existing-link"}, []string{"/usr/bin/true"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "app.go")); !os.IsNotExist(err) {
		t.Fatal("selected deletion not applied")
	}
	if _, err := undoSelectedChange(ws, state.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(ws, "existing-link")); err != nil || got != outside {
		t.Fatal("original symlink target text not restored")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside preserved" {
		t.Fatal("external target touched")
	}
}

func TestSelectiveApplyRejectsSourceChangeDuringSuccessfulCheck(t *testing.T) {
	ws := changeTestRepo(t)
	state, err := applySelectedChange(ws, "agent-1", "main", applyTestHead(t, ws, "agent-1"), []string{"app.go"},
		[]string{"git", "update-ref", "refs/heads/agent-1", applyTestHead(t, ws, "main")}, 10*time.Second)
	if err == nil || state == nil || state.Verification.Status != "candidate_changed" || state.Status != "verification_failed" {
		t.Fatalf("new source head inherited an old check: %+v, %v", state, err)
	}
}

func TestSelectiveApplyCLIAndJournalScope(t *testing.T) {
	ws := changeTestRepo(t)
	head := applyTestHead(t, ws, "agent-1")
	code, err := ReviewCLI([]string{"apply", "agent-1", "--workspace", ws, "--head", head, "--file", "app.go", "--verify", `["/usr/bin/true"]`, "--json"})
	if code != 0 || err != nil {
		t.Fatalf("CLI apply: %d %v", code, err)
	}
	paths, err := filepath.Glob(filepath.Join(ws, ".irrigator/applied/*.json"))
	journals := []string{}
	for _, p := range paths {
		if validApplyID(strings.TrimSuffix(filepath.Base(p), ".json")) {
			journals = append(journals, p)
		}
	}
	if err != nil || len(journals) != 1 {
		t.Fatal("journal missing")
	}
	id := strings.TrimSuffix(filepath.Base(journals[0]), ".json")
	for _, action := range []string{"applied", "undo"} {
		if code, err := ReviewCLI([]string{action, id, "--workspace", ws, "--json"}); code != 0 || err != nil {
			t.Fatalf("CLI %s: %d %v", action, code, err)
		}
	}
	data, err := os.ReadFile(journals[0])
	if err != nil {
		t.Fatal(err)
	}
	var state appliedChange
	if err := json.Unmarshal(data, &state); err != nil || state.Status != "undone" {
		t.Fatalf("undo journal: %+v %v", state, err)
	}
	if _, err := undoSelectedChange(ws, "../../outside"); err == nil {
		t.Fatal("unsafe journal ID accepted")
	}
}
