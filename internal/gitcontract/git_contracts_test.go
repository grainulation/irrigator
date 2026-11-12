package gitcontract

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/runfixture"
)

func gitContractTestOpen(t *testing.T, repo string) *gitContractRepo {
	t.Helper()
	g, err := gitContractOpen(repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g
}
func gitContractCandidate(t *testing.T, g *gitContractRepo, base string) (string, string) {
	t.Helper()
	w := runfixture.FixtureWorktree(t, g.repo, base)
	runfixture.FixtureWrite(t, w, "packages/a/source.txt", "verified candidate\n")
	c, err := g.capture(w, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	oid, err := g.commit(c.Tree, base, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	return oid, w
}
func TestGitContractQuarantinePreservesStagedDirtyAndSalvage(t *testing.T) {
	repo, _ := runfixture.FixtureRepo(t)
	runfixture.FixtureWrite(t, repo, ".gitignore", "*.log\n")
	runfixture.FixtureGit(t, repo, "add", ".")
	runfixture.FixtureGit(t, repo, "commit", "-qm", "ignore logs")
	base := runfixture.FixtureGit(t, repo, "rev-parse", "HEAD")
	g := gitContractTestOpen(t, repo)
	w := runfixture.FixtureWorktree(t, repo, base)
	runfixture.FixtureWrite(t, w, "packages/a/source.txt", "staged value\n")
	runfixture.FixtureGit(t, w, "add", "packages/a/source.txt")
	runfixture.FixtureWrite(t, w, "packages/a/source.txt", "dirty value\n")
	runfixture.FixtureWrite(t, w, "untracked.txt", "untracked\n")
	runfixture.FixtureWrite(t, w, "crash.log", "salvaged\n")
	runfixture.FixtureWrite(t, w, "excluded.log", "not selected\n")
	indexPath := runfixture.FixtureGit(t, w, "rev-parse", "--path-format=absolute", "--git-path", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.quarantine("dirty_fixture", w, base, []string{"crash.log"})
	if err != nil {
		t.Fatal(err)
	}
	runfixture.FixtureGit(t, repo, "gc", "--prune=now")
	if got := runfixture.FixtureGit(t, repo, "show", r.RecoveryRef+":working-tree/packages/a/source.txt"); got != "dirty value" {
		t.Fatal("dirty content missing")
	}
	if got := runfixture.FixtureGit(t, repo, "show", r.RecoveryRef+":working-tree/untracked.txt"); got != "untracked" {
		t.Fatal("untracked content missing")
	}
	if got := runfixture.FixtureGit(t, repo, "show", r.RecoveryRef+":working-tree/crash.log"); got != "salvaged" {
		t.Fatal("selected salvage missing")
	}
	if _, err = g.git(nil, "cat-file", "-e", r.RecoveryRef+":working-tree/excluded.log"); err == nil {
		t.Fatal("unselected ignored content captured")
	}
	raw, err := gitContractRun(repo, nil, nil, "show", r.RecoveryRef+":original-index")
	if err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(t.TempDir(), "index")
	if err = os.WriteFile(idx, raw, 0600); err != nil {
		t.Fatal(err)
	}
	staged, err := gitContractRun(w, []string{"GIT_INDEX_FILE=" + idx}, nil, "show", ":packages/a/source.txt")
	if err != nil || string(staged) != "staged value\n" {
		t.Fatalf("staged object was not retained through GC: %s %v", staged, err)
	}
	after, _ := os.ReadFile(indexPath)
	if string(after) != string(before) {
		t.Fatal("capture modified source index")
	}
	if _, err = os.Stat(w); err != nil {
		t.Fatal("worktree removed")
	}
	if _, err = g.quarantine("dirty_fixture", w, base, nil); err == nil {
		t.Fatal("quarantine ref overwritten")
	}
}
func TestGitContractQuarantineRejectsSymlinkAndGitlink(t *testing.T) {
	for _, mode := range []string{"symlink", "gitlink", "split"} {
		t.Run(mode, func(t *testing.T) {
			repo, base := runfixture.FixtureRepo(t)
			g := gitContractTestOpen(t, repo)
			w := runfixture.FixtureWorktree(t, repo, base)
			switch mode {
			case "symlink":
				if err := os.Symlink("/etc/hosts", filepath.Join(w, "escape")); err != nil {
					t.Fatal(err)
				}
			case "gitlink":
				runfixture.FixtureGit(t, w, "update-index", "--add", "--cacheinfo", "160000,"+base+",module")
			case "split":
				runfixture.FixtureGit(t, w, "update-index", "--split-index")
			}
			if _, err := g.quarantine(mode, w, base, nil); err == nil {
				t.Fatal("unsupported capture accepted")
			}
			if runfixture.FixtureGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/irrigator/quarantine") != "" {
				t.Fatal("failed capture published ref")
			}
		})
	}
}
func TestGitContractPrepareRequiresCleanPassingVerification(t *testing.T) {
	for _, mode := range []string{"passing", "failed", "mutating"} {
		t.Run(mode, func(t *testing.T) {
			repo, base := runfixture.FixtureRepo(t)
			g := gitContractTestOpen(t, repo)
			candidate, _ := gitContractCandidate(t, g, base)
			target := "refs/irrigator/integration/main"
			runfixture.FixtureGit(t, repo, "update-ref", target, base)
			check := []string{"/bin/sh", "-c", "test \"$(cat packages/a/source.txt)\" = 'verified candidate'"}
			if mode == "failed" {
				check = []string{"/bin/sh", "-c", "exit 1"}
			}
			if mode == "mutating" {
				check = []string{"/bin/sh", "-c", "echo altered > README.md"}
			}
			p, err := g.prepare("candidate", candidate, base, target, check, 5*time.Second)
			if mode != "passing" {
				if err == nil {
					t.Fatal("invalid verification accepted")
				}
				if _, err = g.git(nil, "show-ref", "--verify", "refs/irrigator/candidates/candidate"); err == nil {
					t.Fatal("failed check published candidate")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Tree != runfixture.FixtureGit(t, repo, "rev-parse", candidate+"^{tree}") {
				t.Fatal("verification not bound to candidate tree")
			}
			if _, err = g.settle("candidate", false); err == nil || err.Error() != "ACCEPTANCE_REQUIRED" {
				t.Fatalf("settled without acceptance: %v", err)
			}
			if _, err = g.accept("candidate"); err != nil {
				t.Fatal(err)
			}
			if _, err = g.settle("candidate", false); err != nil {
				t.Fatal(err)
			}
			if runfixture.FixtureGit(t, repo, "rev-parse", target) != candidate || runfixture.FixtureGit(t, repo, "rev-parse", "HEAD") != base {
				t.Fatal("wrong ref advanced")
			}
			if _, err = g.settle("candidate", true); err != nil {
				t.Fatal("settlement receipt not idempotent", err)
			}
		})
	}
}
func TestGitContractSettlementCrashReconciliationAndStaleTarget(t *testing.T) {
	for _, mode := range []string{"pending", "advanced", "stale", "missing_intent"} {
		t.Run(mode, func(t *testing.T) {
			repo, base := runfixture.FixtureRepo(t)
			g := gitContractTestOpen(t, repo)
			candidate, _ := gitContractCandidate(t, g, base)
			target := "refs/irrigator/integration/main"
			runfixture.FixtureGit(t, repo, "update-ref", target, base)
			p, err := g.prepare("candidate", candidate, base, target, []string{"/bin/sh", "-c", "test -f packages/a/source.txt"}, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = g.accept("candidate"); err != nil {
				t.Fatal(err)
			}
			intent := gitSettlementIntent{Schema: gitContractSchema, ID: p.ID, PreparedDigest: p.Digest, Target: p.Target, Expected: p.Base, Candidate: p.Candidate}
			if mode != "missing_intent" {
				if err = g.record(p.ID, "intent", intent); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "advanced", "missing_intent":
				runfixture.FixtureGit(t, repo, "update-ref", target, candidate, base)
			case "stale":
				other, err := g.commit(runfixture.FixtureGit(t, repo, "rev-parse", base+"^{tree}"), base, "out-of-band")
				if err != nil {
					t.Fatal(err)
				}
				runfixture.FixtureGit(t, repo, "update-ref", target, other, base)
			}
			before := runfixture.FixtureGit(t, repo, "rev-parse", target)
			receipt, err := g.settle("candidate", true)
			if mode == "advanced" {
				if err != nil || receipt.State != "settled" {
					t.Fatalf("completed CAS not reconciled: %+v %v", receipt, err)
				}
			} else {
				if err == nil {
					t.Fatal("recovery advanced or authorized unsupported state")
				}
				if runfixture.FixtureGit(t, repo, "rev-parse", target) != before {
					t.Fatal("recovery changed target")
				}
				if mode == "stale" && !strings.Contains(err.Error(), "STALE_TARGET_BASE") {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestGitContractRejectsDeveloperTargetAndTamperedAcceptance(t *testing.T) {
	// `true` lives in /usr/bin on macOS and /bin or /usr/bin on Linux.
	truePath, lookErr := exec.LookPath("true")
	if lookErr != nil {
		t.Skip("no true command on PATH")
	}
	repo, base := runfixture.FixtureRepo(t)
	g := gitContractTestOpen(t, repo)
	candidate, _ := gitContractCandidate(t, g, base)
	if _, err := g.prepare("bad", candidate, base, "refs/heads/main", []string{truePath}, 30*time.Second); err == nil {
		t.Fatal("developer target accepted")
	}
	if _, err := g.quarantine("../escape", repo, base, nil); err == nil {
		t.Fatal("unsafe id accepted")
	}
	target := "refs/irrigator/integration/main"
	runfixture.FixtureGit(t, repo, "update-ref", target, base)
	p, err := g.prepare("candidate", candidate, base, target, []string{truePath}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(gitAcceptanceRecord{Schema: gitContractSchema, ID: p.ID, PreparedDigest: strings.Repeat("0", 64)})
	if err = os.WriteFile(filepath.Join(g.common, "irrigator/git-contracts/candidate.accept.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = g.settle("candidate", false); err == nil || err.Error() != "ACCEPTANCE_REQUIRED" {
		t.Fatal("wrong candidate acceptance honored", err)
	}
}
