//go:build integration

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/debug"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Replay harness and gate for the experimental replay fixtures (see
// replay_fixture.go). A fixture is rebuilt from its recorded data only: the
// harness derives a plan and a fault driver from the fixture's statuses, codes
// and flags, runs it through the real worker/executor path (stub provider
// process, real validation, real cancellation, real killed-worker recovery),
// and compares the re-exported fixture with the recorded one. Outcomes the
// executor cannot produce (an uncertain outcome on the mock backend, or an
// uncertain reason with no stub that reproduces it) fall back to the cell
// transition reducer; replayPathOf names which path each task takes.

func loadReplayFixture(path string) (debug.ReplayFixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return debug.ReplayFixture{}, err
	}
	return debug.DecodeReplayFixture(raw)
}

// replayDiff exports the replayed run with the same exporter and returns every
// field that differs from the recorded fixture. Events are compared per task,
// because sibling interleaving in a parallel run is nondeterministic.
func replayDiff(want, got debug.ReplayFixture) []string {
	var diffs []string
	if !reflect.DeepEqual(want.Tasks, got.Tasks) {
		for i := range want.Tasks {
			if i >= len(got.Tasks) || !reflect.DeepEqual(want.Tasks[i], got.Tasks[i]) {
				var g debug.ReplayTask
				if i < len(got.Tasks) {
					g = got.Tasks[i]
				}
				diffs = append(diffs, fmt.Sprintf("task %s: recorded %+v replayed %+v", want.Tasks[i].ID, want.Tasks[i], g))
			}
		}
		if len(want.Tasks) != len(got.Tasks) {
			diffs = append(diffs, "task count differs")
		}
	}
	perTask := func(es []debug.ReplayEvent) map[string][]string {
		m := map[string][]string{}
		for _, e := range es {
			m[e.Task] = append(m[e.Task], e.Kind)
		}
		return m
	}
	w, g := perTask(want.Events), perTask(got.Events)
	var ids []string
	for id := range w {
		ids = append(ids, id)
	}
	for id := range g {
		if _, ok := w[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !reflect.DeepEqual(w[id], g[id]) {
			diffs = append(diffs, fmt.Sprintf("events %s: recorded %v replayed %v", id, w[id], g[id]))
		}
	}
	if !reflect.DeepEqual(want.Recovery, got.Recovery) {
		diffs = append(diffs, fmt.Sprintf("recovery decision: recorded %+v replayed %+v", want.Recovery, got.Recovery))
	}
	return diffs
}

// TestReplayGate is the acceptance gate: every fixture in dev/ and heldout/
// must replay to its recorded terminal states and recovery decision. A change
// is accepted only when BOTH sets pass; each set is reported separately so a
// regression names the set that caught it.
func TestReplayGate(t *testing.T) {
	passed := map[string]int{}
	total := map[string]int{}
	for _, set := range []string{"dev", "heldout"} {
		files, _ := filepath.Glob(filepath.Join(runfixture.ReplayDir(), set, "*.json"))
		sort.Strings(files)
		if len(files) == 0 {
			t.Fatalf("replay gate: set %q has no fixtures in %s", set, runfixture.ReplayDir())
		}
		for _, f := range files {
			total[set]++
			name := set + "/" + strings.TrimSuffix(filepath.Base(f), ".json")
			ok := t.Run(name, func(t *testing.T) {
				fx, err := loadReplayFixture(f)
				if err != nil {
					t.Fatal("invalid or non-redacted fixture:", err)
				}
				if diffs := replayRoundTrip(t, fx); len(diffs) > 0 {
					t.Fatalf("replay regressed:\n  %s", strings.Join(diffs, "\n  "))
				}
			})
			if ok {
				passed[set]++
			}
		}
	}
	t.Logf("REPLAY-GATE dev=%d/%d heldout=%d/%d", passed["dev"], total["dev"], passed["heldout"], total["heldout"])
}

// TestReplayGateDetectsRegression proves the gate can fail: each mutation of a
// recorded terminal state or recovery decision must produce a diff.
func TestReplayGateDetectsRegression(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(runfixture.ReplayDir(), "dev", "*.json"))
	if len(files) == 0 {
		t.Skip("no dev fixtures")
	}
	fx, err := loadReplayFixture(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if d := replayRoundTrip(t, fx); len(d) > 0 {
		t.Fatalf("baseline must pass: %v", d)
	}
	mutations := map[string]func(f *debug.ReplayFixture){
		"status": func(f *debug.ReplayFixture) {
			f.Tasks[0].Status = "verified"
			f.Tasks[len(f.Tasks)-1].Status = "failed"
		},
		"attempt":  func(f *debug.ReplayFixture) { f.Tasks[0].Attempt += 3 },
		"recovery": func(f *debug.ReplayFixture) { f.Recovery.NextSteps = append(f.Recovery.NextSteps, "extra") },
		"events": func(f *debug.ReplayFixture) {
			f.Events = append(f.Events, debug.ReplayEvent{Kind: "verified", Task: f.Tasks[0].ID})
		},
	}
	replayed := debug.ExportReplayFixture(replayRun(t, fx), fx.Scenario)
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(fx)
			var m debug.ReplayFixture
			_ = json.Unmarshal(raw, &m)
			mutate(&m)
			if len(replayDiff(m, replayed)) == 0 {
				t.Fatal("mutation was not detected")
			}
		})
	}
}

func replayRoundTrip(t *testing.T, fx debug.ReplayFixture) []string {
	t.Helper()
	return replayDiff(fx, debug.ExportReplayFixture(replayRun(t, fx), fx.Scenario))
}

// TestReplayReducerFallbackIsLabelled covers the one honest gap: the mock
// backend cannot produce an uncertain outcome, so such a fixture replays through
// the cell transition reducer and replayPathOf says so.
func TestReplayReducerFallbackIsLabelled(t *testing.T) {
	fx := debug.ReplayFixture{Schema: debug.ReplaySchema, Scenario: "mock-uncertain-reducer", Backend: "mock", Events: []debug.ReplayEvent{}, Tasks: []debug.ReplayTask{
		{ID: "t1", DependsOn: []string{}, Status: "uncertain", Attempt: 1, Generation: 1, ErrorCode: "outcome_uncertain_invalid_outcome"}}}
	if err := debug.ValidateReplayFixture(fx); err != nil {
		t.Fatal(err)
	}
	if got := replayPathOf(fx); got != "reducer" {
		t.Fatalf("path %s", got)
	}
	replayed := debug.ExportReplayFixture(replayRun(t, fx), fx.Scenario)
	if replayed.Tasks[0].Status != "uncertain" || replayed.Tasks[0].ErrorCode != "outcome_uncertain_invalid_outcome" {
		t.Fatalf("reducer replay: %+v", replayed.Tasks[0])
	}
}
