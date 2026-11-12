package debug

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestDebugFixtureHelpIsMarkedExperimental(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"fixture", "--help"}, {"fixture", "export", "--help"}} {
		var out bytes.Buffer
		if code, err := DebugRun(args, &out); code != 0 || err != nil || !strings.Contains(out.String(), "experimental") {
			t.Fatalf("%v: %d %v %q", args, code, err, out.String())
		}
	}
}

func TestReplayDecoderRejectsUnknownVersionsAndFields(t *testing.T) {
	good, err := os.ReadFile(filepath.Join(runfixture.ReplayDir(), "dev", "failed-validation-blocks-dependent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReplayFixture(good); err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(good, &m)
		f(m)
		out, _ := json.Marshal(m)
		return out
	}
	task0 := func(m map[string]any) map[string]any { return m["tasks"].([]any)[0].(map[string]any) }
	cases := map[string][]byte{
		"future version":   mutate(func(m map[string]any) { m["schema"] = "irrigator.replay-fixture/v2" }),
		"no version":       mutate(func(m map[string]any) { delete(m, "schema") }),
		"unknown top":      mutate(func(m map[string]any) { m["prompt"] = "free text" }),
		"unknown task":     mutate(func(m map[string]any) { task0(m)["error"] = "free text" }),
		"bad status":       mutate(func(m map[string]any) { task0(m)["status"] = "SENTINEL" }),
		"real task id":     mutate(func(m map[string]any) { task0(m)["id"] = "build-the-thing" }),
		"free text code":   mutate(func(m map[string]any) { task0(m)["error_code"] = "failed_x y" }),
		"bad diag":         mutate(func(m map[string]any) { task0(m)["diagnostic_code"] = "SECRET" }),
		"bad phase":        mutate(func(m map[string]any) { task0(m)["failure_phase"] = "whatever" }),
		"unfixed advice":   mutate(func(m map[string]any) { m["recovery"].(map[string]any)["next_steps"] = []any{"rm -rf ~"} }),
		"bad backend":      mutate(func(m map[string]any) { m["backend"] = "custom" }),
		"event unknown id": mutate(func(m map[string]any) { m["events"] = []any{map[string]any{"kind": "started", "task": "t99"}} }),
		"truncated":        good[:len(good)/2],
		"trailing":         append(append([]byte{}, good...), []byte(`{"x":1}`)...),
	}
	for name, raw := range cases {
		if _, err := DecodeReplayFixture(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestReplayCommittedFixtureCoverage pins what the committed set must contain,
// so the executor-path claim cannot silently erode.
func TestReplayCommittedFixtureCoverage(t *testing.T) {
	need := map[string]string{ // fixture name -> replay path that must hold
		"uncertain-provider-timeout":                    "executor",
		"uncertain-killed-worker":                       "executor",
		"cancelled-in-flight-provider":                  "executor",
		"uncertain-malformed-response":                  "executor",
		"verified-cost-unknown":                         "executor",
		"failed-provider-auth":                          "executor",
		"failed-provider-error-with-cost":               "executor",
		"failed-provider-session-budget":                "executor",
		"spawn-waiting-parent-failed-child":             "executor",
		"spawn-nested-waiting-cancelled-child":          "executor",
		"turn-failed-helper-blocks-waited-continuation": "executor",
		"managed-mock-failed-validation":                "executor",
	}
	codes := map[string]bool{}
	for _, set := range []string{"dev", "heldout"} {
		files, _ := filepath.Glob(filepath.Join(runfixture.ReplayDir(), set, "*.json"))
		for _, f := range files {
			fx, err := loadReplayFixture(f)
			if err != nil {
				t.Fatal(err)
			}
			if want, ok := need[fx.Scenario]; ok {
				if got := replayPathOf(fx); got != want {
					t.Errorf("%s replays via %s, want %s", fx.Scenario, got, want)
				}
				delete(need, fx.Scenario)
			}
			for _, tk := range fx.Tasks {
				if tk.ErrorCode != "" {
					codes[tk.ErrorCode] = true
				}
			}
		}
	}
	for name := range need {
		t.Errorf("committed fixture %s is missing", name)
	}
	for _, c := range []string{"failed_error_not_projected", "failed_provider_authentication_required", "failed_provider_error", "failed_provider_session_budget_exhausted", "outcome_uncertain_deadline", "outcome_uncertain_cancelled", "outcome_uncertain_provider_reported", "outcome_uncertain_unclassified", "cancelled"} {
		if !codes[c] {
			t.Errorf("no committed fixture carries error code %s", c)
		}
	}
}
