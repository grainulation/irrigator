package branchreview

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func requireSafeReviewOutput(t *testing.T, text string) {
	t.Helper()
	if !utf8.ValidString(text) {
		t.Fatal("terminal output contains invalid UTF-8")
	}
	for _, r := range text {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			t.Fatalf("terminal output contains active U+%04X", r)
		}
	}
}

func requireReviewLine(t *testing.T, text, want string) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("missing exact display line %q in %q", want, text)
}

func TestReviewBackendPreservesRawDiffAndCLIPrintsEscapesOnce(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "config", "core.autocrlf", "false")
	reviewTestGit(t, ws, "switch", "-qc", "hostile-review")
	payload := []byte("literal \\r\r\nembedded a\rb\r\nunsafe \x1b]52;c;data\a \u202e \xff\r\n")
	if err := os.WriteFile(filepath.Join(ws, "hostile.txt"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "add", "hostile.txt")
	reviewTestGit(t, ws, "commit", "-qm", "hostile content fixture")
	head, err := changeRev(ws, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "switch", "-q", "main")
	backend := reviewTUIBackend{ws: ws, base: "main"}
	raw, err := backend.Diff("hostile-review", head)
	if err != nil {
		t.Fatal(err)
	}
	for _, original := range [][]byte{[]byte("+literal \\r\r\n"), []byte("+embedded a\rb\r\n"), []byte("\x1b]52;c;data\a"), []byte("\u202e"), {0xff}} {
		if !bytes.Contains([]byte(raw), original) {
			t.Fatalf("TUI backend escaped or lost raw Git bytes %q: %q", original, raw)
		}
	}
	shown, err := changeDiff(ws, "hostile-review", "main", "", "")
	if err != nil {
		t.Fatal(err)
	}
	requireSafeReviewOutput(t, shown)
	requireReviewLine(t, shown, `+literal \\r\r`)
	requireReviewLine(t, shown, `+embedded a\rb\r`)
	requireReviewLine(t, shown, `+unsafe \e]52;c;data\a \u202E \xFF\r`)
	if _, err := backend.Diff("hostile-review", strings.Repeat("0", 40)); err == nil {
		t.Fatal("TUI accepted a head different from the reviewed head")
	}
}

func TestReviewSnapshotKeepsRawBytesWhilePresentationEscapesOnce(t *testing.T) {
	ws := changeTestRepo(t)
	payload := "literal \\r\r\nunsafe \x1b]52;c;data\a \u202e\r\n"
	if err := os.WriteFile(filepath.Join(ws, "snapshot.txt"), []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := rawUncommittedDiff(ws, "snapshot.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "+literal \\r\r\n") || !strings.Contains(raw, "\x1b]52") {
		t.Fatalf("untracked raw fallback escaped input: %q", raw)
	}
	shown, err := uncommittedDiff(ws, "snapshot.txt")
	if err != nil {
		t.Fatal(err)
	}
	requireSafeReviewOutput(t, shown)
	requireReviewLine(t, shown, `+literal \\r\r`)
	snapshot, err := captureReview(ws, "")
	if err != nil {
		t.Fatal(err)
	}
	file := reviewFind(t, snapshot, "snapshot.txt")
	if file.After != payload || !strings.Contains(file.Diff, "+literal \\r\r\n") || !strings.Contains(file.Diff, "\x1b]52") {
		t.Fatal("presentation escaping changed durable review content")
	}
	out := reviewTestOut(t)
	if err := writeReview(out, snapshot); err != nil {
		t.Fatal(err)
	}
	saved, err := readReview(out)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != snapshot.ID || reviewFind(t, saved, "snapshot.txt").Diff != file.Diff {
		t.Fatal("review snapshot identity or raw diff changed on persistence")
	}
	printed := localReviewCLIOutput(t, []string{"show", out})
	requireSafeReviewOutput(t, printed)
	requireReviewLine(t, printed, `+literal \\r\r`)
}
