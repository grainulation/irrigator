package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func limitFixture(t *testing.T) (string, time.Time) {
	t.Helper()
	root := runfixture.New(t, runfixture.Task("a", 0))
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Attempt = 1; s.Tasks["a"].Status = "failed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "cells", "a"), 0700); err != nil {
		t.Fatal(err)
	}
	return root, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
}

func writeLimit(t *testing.T, root, text string, observed time.Time) {
	t.Helper()
	path := filepath.Join(root, "cells", "a", "1.stderr.log")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, observed, observed); err != nil {
		t.Fatal(err)
	}
}

func TestProviderLimitAdvisoryExpiryAndNoRawSecrets(t *testing.T) {
	root, observed := limitFixture(t)
	writeLimit(t, root, "HTTP 429; retry-after: 120s; key=DO_NOT_OUTPUT_ME", observed)
	got, err := DiagnoseLimit(root, "a", 1, observed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cooldown_advisory" || got.ResetAt == nil || !got.ResetAt.Equal(observed.Add(2*time.Minute)) || got.AutomaticRetry {
		t.Fatalf("wrong advisory: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "DO_NOT_OUTPUT_ME") {
		t.Fatal("provider prose leaked")
	}
	got, err = DiagnoseLimit(root, "a", 1, observed.Add(3*time.Minute))
	if err != nil || got.Status != "reset_elapsed_advisory" {
		t.Fatalf("expiry: %+v %v", got, err)
	}
}

func TestProviderLimitAmbiguityAndBudgetSeparation(t *testing.T) {
	root, observed := limitFixture(t)
	writeLimit(t, root, `rate_limit_error; resets_at: 1795003260; retry after 120s`, observed)
	got, err := DiagnoseLimit(root, "a", 1, observed)
	if err != nil {
		t.Fatal(err)
	}
	if got.ResetBasis != "conflicting_estimates" || got.ResetAt != nil {
		t.Fatalf("conflicting resets hidden: %+v", got)
	}
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].DiagnosticCode = model.DiagProviderSessionBudget; return nil }); err != nil {
		t.Fatal(err)
	}
	got, err = DiagnoseLimit(root, "a", 1, observed)
	if err != nil || got.Status != "operation_or_session_budget" || got.ResetAt != nil {
		t.Fatalf("budget confused with quota: %+v %v", got, err)
	}
}

func TestProviderLimitRejectsSymlinkAndBadAttempt(t *testing.T) {
	root, observed := limitFixture(t)
	if err := os.Symlink(filepath.Join(root, "state.json"), filepath.Join(root, "cells", "a", "1.stderr.log")); err != nil {
		t.Fatal(err)
	}
	got, err := DiagnoseLimit(root, "a", 1, observed)
	if err != nil || got.Status != "unavailable" || len(got.ReadErrors) != 1 {
		t.Fatalf("unsafe source accepted: %+v %v", got, err)
	}
	if _, err := DiagnoseLimit(root, "a", 2, observed); err == nil {
		t.Fatal("accepted future attempt")
	}
	if _, err := DiagnoseLimit(root, "../a", 1, observed); err == nil {
		t.Fatal("accepted path task")
	}
}

func TestLimitResetClockFormats(t *testing.T) {
	observed := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{`reset_at: "2026-11-18T13:00:00+00:00"`, `resetsAt: 1795006800`, `resets_at: 1795006800000`, `resets in 1h`} {
		got := limitResetCandidates(raw, observed)
		if len(got) != 1 || !got[0].Equal(observed.Add(time.Hour)) {
			t.Fatalf("%q: %v", raw, got)
		}
	}
	if got := limitResetCandidates("resets in 9999999h", observed); len(got) != 0 {
		t.Fatal("unbounded reset accepted")
	}
}

func TestProviderLimitAllowedWindowIsNotARejection(t *testing.T) {
	root, observed := limitFixture(t)
	writeLimit(t, root, `{"type":"rate_limit_event","status":"allowed","resets_at":1795006800}`, observed)
	got, err := DiagnoseLimit(root, "a", 1, observed)
	if err != nil || got.Status != "no_limit_evidence" || got.ResetAt != nil {
		t.Fatalf("allowed window treated as denial: %+v %v", got, err)
	}
}

func TestLimitResetDoesNotAcceptNumericPrefixes(t *testing.T) {
	observed := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	got := limitResetCandidates("HTTP 429; retry-after: 2026-11-18T13:00:00Z", observed)
	if len(got) != 1 || !got[0].Equal(observed.Add(time.Hour)) {
		t.Fatalf("ISO date also interpreted as duration: %v", got)
	}
	for _, raw := range []string{"retry after 1.5s", "retry after 120ms", "retry after 1 fortnight", "retry after 1,500s", "retry after 120sfoo"} {
		if got := limitResetCandidates(raw, observed); len(got) != 0 {
			t.Fatalf("unsupported duration %q received estimate: %v", raw, got)
		}
	}
	for _, raw := range []string{"retry after 120 seconds", "retry after 2m", "retry-after: 120"} {
		got := limitResetCandidates(raw, observed)
		if len(got) != 1 || !got[0].Equal(observed.Add(2*time.Minute)) {
			t.Fatalf("complete duration %q not parsed: %v", raw, got)
		}
	}
}
