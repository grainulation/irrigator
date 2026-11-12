package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testPlanHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const otherPlanHash = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// checkpointDoc renders a representative checkpoint document. Empty runID and
// schema omit those fields, which is the legacy shape.
func checkpointDoc(runID, planHash, control string, schema int) string {
	var b strings.Builder
	b.WriteString("{\n")
	if schema > 0 {
		fmt.Fprintf(&b, "  \"schema_version\": %d,\n", schema)
	}
	b.WriteString("  \"version\": 1,\n")
	if runID != "" {
		fmt.Fprintf(&b, "  \"run_id\": %q,\n", runID)
	}
	fmt.Fprintf(&b, "  \"plan\": {\"name\": \"test\", \"tasks\": [{\"id\": \"a\", \"prompt\": \"p\", \"requires\": [\"answer\"]}]},\n")
	fmt.Fprintf(&b, "  \"plan_hash\": %q,\n", planHash)
	fmt.Fprintf(&b, "  \"backend\": \"mock\",\n  \"control\": %q,\n", control)
	b.WriteString("  \"tasks\": {\"a\": {\"spec\": {\"id\": \"a\"}, \"status\": \"pending\", \"attempt\": 0}},\n")
	b.WriteString("  \"events\": []\n}\n")
	return b.String()
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func noStrayTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-backup-") {
			t.Fatalf("temporary backup file left behind: %s", e.Name())
		}
	}
}

func TestEnsureBackupIdempotent(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "state.json")
	v1 := checkpointDoc("run-1", testPlanHash, "ready", 0)
	writeTestFile(t, original, v1)

	// First backup creates state.json.bak
	if err := EnsureBackup(original); err != nil {
		t.Fatalf("first EnsureBackup: %v", err)
	}
	if got := readTestFile(t, original+".bak"); got != v1 {
		t.Fatalf("backup content = %q, want original", got)
	}
	noStrayTemps(t, dir)

	// The live checkpoint moves on (same run and plan); the older backup stays.
	writeTestFile(t, original, checkpointDoc("run-1", testPlanHash, "run", 2))
	if err := EnsureBackup(original); err != nil {
		t.Fatalf("second EnsureBackup: %v", err)
	}
	if got := readTestFile(t, original+".bak"); got != v1 {
		t.Fatalf("backup was overwritten: %q", got)
	}

	// state.v1.json.bak satisfies the backup requirement; no .bak is created.
	dir2 := t.TempDir()
	orig2 := filepath.Join(dir2, "state.json")
	writeTestFile(t, orig2, checkpointDoc("run-2", testPlanHash, "run", 2))
	alias := checkpointDoc("run-2", testPlanHash, "ready", 0)
	writeTestFile(t, filepath.Join(dir2, "state.v1.json.bak"), alias)
	if err := EnsureBackup(orig2); err != nil {
		t.Fatalf("EnsureBackup with v1 alias: %v", err)
	}
	if _, err := os.Stat(orig2 + ".bak"); err == nil {
		t.Fatal("EnsureBackup should have skipped creating .bak when a valid state.v1.json.bak exists")
	}
	if got := readTestFile(t, filepath.Join(dir2, "state.v1.json.bak")); got != alias {
		t.Fatal("v1 alias was modified")
	}
}

func TestEnsureBackupMissingSourceIsNoop(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureBackup(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("EnsureBackup without source: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("unexpected files created: %v", entries)
	}
}

func TestEnsureBackupRefusesNonCheckpointSource(t *testing.T) {
	for name, content := range map[string]string{
		"empty":     "",
		"garbage":   "not json",
		"truncated": checkpointDoc("run-1", testPlanHash, "ready", 2)[:80],
		"no tasks":  `{"version":1,"plan_hash":"` + testPlanHash + `","plan":{"tasks":[]},"tasks":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			original := filepath.Join(dir, "state.json")
			writeTestFile(t, original, content)
			if err := EnsureBackup(original); err == nil {
				t.Fatal("expected refusal to back up a non-checkpoint")
			}
			if _, err := os.Lstat(original + ".bak"); err == nil {
				t.Fatal("a backup of a non-checkpoint was published")
			}
			if got := readTestFile(t, original); got != content {
				t.Fatal("source was modified")
			}
			noStrayTemps(t, dir)
		})
	}
}

// Both backup names receive identical scrutiny. A suspect backup must be
// rejected, preserved byte-for-byte, and never silently replaced.
func TestEnsureBackupRejectsSuspectExistingBackups(t *testing.T) {
	live := checkpointDoc("run-1", testPlanHash, "run", 2)
	valid := checkpointDoc("run-1", testPlanHash, "ready", 0)
	type suspect struct {
		name  string
		setup func(t *testing.T, target string)
		kept  string // expected preserved file content, "" to skip content check
	}
	file := func(content string) func(*testing.T, string) {
		return func(t *testing.T, target string) { writeTestFile(t, target, content) }
	}
	suspects := []suspect{
		{"empty", file(""), ""},
		{"truncated mid-document", file(valid[:len(valid)/2]), valid[:len(valid)/2]},
		{"garbage", file("\x00\x01 not json"), "\x00\x01 not json"},
		{"valid json but not a checkpoint", file(`{"hello":"world"}`), `{"hello":"world"}`},
		{"json array", file(`[1,2,3]`), `[1,2,3]`},
		{"trailing garbage", file(valid + "garbage"), valid + "garbage"},
		{"unsupported version", file(strings.Replace(valid, `"version": 1`, `"version": 9`, 1)), strings.Replace(valid, `"version": 1`, `"version": 9`, 1)},
		{"unrelated plan", file(checkpointDoc("run-1", otherPlanHash, "ready", 0)), checkpointDoc("run-1", otherPlanHash, "ready", 0)},
		{"unrelated run", file(checkpointDoc("run-other", testPlanHash, "ready", 0)), checkpointDoc("run-other", testPlanHash, "ready", 0)},
		{"directory", func(t *testing.T, target string) {
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"symlink to valid checkpoint", func(t *testing.T, target string) {
			real := filepath.Join(filepath.Dir(target), "elsewhere.json")
			writeTestFile(t, real, valid)
			if err := os.Symlink(real, target); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"dangling symlink", func(t *testing.T, target string) {
			if err := os.Symlink(filepath.Join(filepath.Dir(target), "missing"), target); err != nil {
				t.Fatal(err)
			}
		}, ""},
	}
	for _, alias := range []string{"state.json.bak", "state.v1.json.bak"} {
		for _, s := range suspects {
			t.Run(alias+"/"+s.name, func(t *testing.T) {
				dir := t.TempDir()
				original := filepath.Join(dir, "state.json")
				writeTestFile(t, original, live)
				target := filepath.Join(dir, alias)
				s.setup(t, target)
				before, err := os.Lstat(target)
				if err != nil {
					t.Fatal(err)
				}

				err = EnsureBackup(original)
				if !errors.Is(err, errSuspectBackup) {
					t.Fatalf("EnsureBackup = %v, want ErrSuspectBackup", err)
				}
				after, err := os.Lstat(target)
				if err != nil {
					t.Fatalf("suspect backup was removed: %v", err)
				}
				if before.Mode() != after.Mode() || (before.Mode().IsRegular() && before.Size() != after.Size()) {
					t.Fatal("suspect backup was modified")
				}
				if s.kept != "" && readTestFile(t, target) != s.kept {
					t.Fatal("suspect backup content changed")
				}
				if readTestFile(t, original) != live {
					t.Fatal("live checkpoint was modified")
				}
				// Nothing else may be published next to the evidence.
				for _, other := range []string{"state.json.bak", "state.v1.json.bak"} {
					if other == alias {
						continue
					}
					if _, err := os.Lstat(filepath.Join(dir, other)); err == nil {
						t.Fatalf("%s was published beside a suspect backup", other)
					}
				}
				noStrayTemps(t, dir)
			})
		}
	}
}

func TestEnsureBackupAcceptsLegacyShapesAndPreservesThem(t *testing.T) {
	// A legitimate pre-schema backup lacks run_id and schema_version; the live
	// checkpoint later gained both. The older backup must be kept untouched.
	legacy := checkpointDoc("", testPlanHash, "ready", 0)
	live := checkpointDoc("legacy-run", testPlanHash, "run", 2)
	for _, alias := range []string{"state.json.bak", "state.v1.json.bak"} {
		t.Run(alias, func(t *testing.T) {
			dir := t.TempDir()
			original := filepath.Join(dir, "state.json")
			writeTestFile(t, original, live)
			writeTestFile(t, filepath.Join(dir, alias), legacy)
			if err := EnsureBackup(original); err != nil {
				t.Fatalf("legacy backup rejected: %v", err)
			}
			if got := readTestFile(t, filepath.Join(dir, alias)); got != legacy {
				t.Fatal("legacy backup was modified")
			}
			noStrayTemps(t, dir)
		})
	}
	// Live checkpoint without run_id (legacy) also accepts a backup carrying one.
	dir := t.TempDir()
	original := filepath.Join(dir, "state.json")
	writeTestFile(t, original, checkpointDoc("", testPlanHash, "run", 0))
	writeTestFile(t, original+".bak", checkpointDoc("run-x", testPlanHash, "ready", 0))
	if err := EnsureBackup(original); err != nil {
		t.Fatalf("backup with run_id vs legacy live rejected: %v", err)
	}
}

func TestEnsureBackupConcurrentPublication(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "state.json")
	doc := checkpointDoc("run-1", testPlanHash, "ready", 0)
	writeTestFile(t, original, doc)

	const workers = 24
	start := make(chan struct{})
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = EnsureBackup(original)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	if got := readTestFile(t, original+".bak"); got != doc {
		t.Fatalf("published backup differs from source: %q", got)
	}
	noStrayTemps(t, dir)

	// Idempotent afterwards, even once the live checkpoint advances.
	writeTestFile(t, original, checkpointDoc("run-1", testPlanHash, "run", 2))
	if err := EnsureBackup(original); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, original+".bak"); got != doc {
		t.Fatal("backup changed after later checkpoint")
	}
}

// A crash between creating the temporary file and publishing leaves only an
// ignorable temporary. It is neither mistaken for a backup nor disturbed.
func TestEnsureBackupInterruptedTempIsIgnored(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "state.json")
	doc := checkpointDoc("run-1", testPlanHash, "ready", 0)
	writeTestFile(t, original, doc)
	stale := filepath.Join(dir, ".state-backup-crashed.tmp")
	writeTestFile(t, stale, doc[:20])

	if err := EnsureBackup(original); err != nil {
		t.Fatalf("EnsureBackup with stale temp: %v", err)
	}
	if got := readTestFile(t, original+".bak"); got != doc {
		t.Fatalf("backup content = %q, want complete source", got)
	}
	if got := readTestFile(t, stale); got != doc[:20] {
		t.Fatal("unrelated stale temporary was modified")
	}
}

func TestEnsureBackupPublishedFileIsPrivateRegular(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "state.json")
	writeTestFile(t, original, checkpointDoc("run-1", testPlanHash, "ready", 0))
	if err := EnsureBackup(original); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(original + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("backup mode = %v, want private regular file", info.Mode())
	}
}
