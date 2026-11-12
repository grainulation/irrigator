package debug

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/telemetry"
)

// TestReplayCommittedFixturesRedacted re-validates every committed fixture and
// scans the raw bytes for the planted sentinels and for secret-like content or
// absolute home paths, independently of the allowlist.
func TestReplayCommittedFixturesRedacted(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(runfixture.ReplayDir(), "*", "*.json"))
	if len(files) < 3 {
		t.Fatalf("expected at least 3 fixtures, found %d", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if hit := runfixture.ReplayContainsSentinel(raw); hit != "" {
			t.Errorf("%s contains %s", f, hit)
		}
		if telemetry.RedactOtelText(string(raw)) != string(raw) {
			t.Errorf("%s contains a URL or header value", f)
		}
		if hits := ReplaySecretScan(raw); len(hits) > 0 {
			t.Errorf("%s matches secret/path pattern(s): %v", f, hits)
		}
		if _, err := DecodeReplayFixture(raw); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// TestReplaySecretScanCatchesEachPlantedKind keeps the independent scanner
// honest: a prompt-embedded token, an absolute home path, a URL, a credential
// assignment and a long hex digest must each be flagged.
func TestReplaySecretScanCatchesEachPlantedKind(t *testing.T) {
	for name, raw := range map[string]string{
		"token":     `{"a":"` + runfixture.SentinelToken + `"}`,
		"home path": `{"a":"` + runfixture.SentinelHomePath() + `"}`,
		"users":     `{"a":"/Users/someone/project/file.go"}`,
		"url":       `{"a":"https://example.invalid/x"}`,
		"assign":    `{"a":"api_key=abc"}`,
		"hex":       `{"a":"` + strings.Repeat("ab12", 10) + `"}`,
		"traversal": `{"a":"../../etc/passwd"}`,
	} {
		if len(ReplaySecretScan([]byte(raw))) == 0 {
			t.Errorf("%s not flagged", name)
		}
	}
	for _, f := range []string{"failed_provider_session_budget_exhausted", "uncertain_reconcile_externally"} {
		if len(ReplaySecretScan([]byte(`{"a":"`+f+`"}`))) != 0 {
			t.Errorf("false positive on fixed code %s", f)
		}
	}
}
