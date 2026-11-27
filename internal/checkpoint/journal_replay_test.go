package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

func withVerifyMode(t *testing.T, mode string) {
	t.Helper()
	old := journalVerifyMode
	journalVerifyMode = mode
	t.Cleanup(func() { journalVerifyMode = old })
}

func dropJournalCache(t *testing.T, root string) {
	t.Helper()
	key, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	committedCache.Lock()
	delete(committedCache.entries, key)
	committedCache.Unlock()
}

// staleRead commits change from a warm cache, then reads the result while the
// cache still holds the previous head, which is exactly what a warm replay is.
func staleRead(t *testing.T, root string, change func(*model.State)) (*model.State, error, uint64) {
	t.Helper()
	if _, err := ReadState(root); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if err := Update(root, func(s *model.State) error { change(s); return nil }); err != nil {
		t.Fatalf("commit: %v", err)
	}
	before := checkpointIncrementalReplays.Load()
	s, err := ReadState(root)
	return s, err, checkpointIncrementalReplays.Load() - before
}

func coldRead(t *testing.T, root string) (*model.State, error) {
	t.Helper()
	dropJournalCache(t, root)
	return ReadState(root)
}

func TestIncrementalReplayMatchesColdDecode(t *testing.T) {
	withVerifyMode(t, journalVerifyDual)
	steps := []struct {
		name        string
		incremental bool
		change      func(*model.State)
	}{
		{"control scalar", true, func(s *model.State) { s.Control = "pause" }},
		{"task status", true, func(s *model.State) { s.Tasks["a"].Status = "running"; s.Tasks["a"].Attempt = 1 }},
		{"task error", true, func(s *model.State) { s.Tasks["b"].Error = "boom" }},
		{"event appended", true, func(s *model.State) { s.Event("later", "a", map[string]any{"n": 1, "nested": []any{"x", 2.5}}) }},
		{"two events", true, func(s *model.State) { s.Event("one", "", nil); s.Event("two", "b", "text") }},
		{"empty notes map", true, func(s *model.State) { s.Tasks["a"].Notes = map[string]string{} }},
		{"empty children slice", true, func(s *model.State) { s.Tasks["a"].Children = []string{} }},
		{"empty result map", true, func(s *model.State) { s.Tasks["b"].Result = map[string]string{} }},
		{"notes set", true, func(s *model.State) { s.Tasks["a"].Notes = map[string]string{"k": "v"} }},
		{"notes key removed", false, func(s *model.State) { delete(s.Tasks["a"].Notes, "k") }},
		{"usage pointer set", true, func(s *model.State) { s.Tasks["b"].Usage = &model.TaskUsage{InputTokens: 7} }},
		{"usage pointer cleared", true, func(s *model.State) { s.Tasks["b"].Usage = nil }},
	}
	root := journalFixture(t)
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			got, err, replays := staleRead(t, root, step.change)
			if err != nil {
				t.Fatalf("incremental read: %v", err)
			}
			if step.incremental && replays == 0 {
				t.Fatal("expected the warm replay to take the incremental path")
			}
			if !step.incremental && replays != 0 {
				t.Fatal("expected this shape to fall back to the full path")
			}
			want, err := coldRead(t, root)
			if err != nil {
				t.Fatalf("cold read: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("incremental replay and cold decode produced different states")
			}
		})
	}
}

func managedJournalRoot(t *testing.T) string {
	t.Helper()
	s := sharedProfileState(t, 12, true)
	plan, err := json.Marshal(s.Plan)
	if err != nil {
		t.Fatal(err)
	}
	s.PlanHash = fmt.Sprintf("%x", sha256.Sum256(plan))
	s.SchemaVersion = model.CurrentSchemaVersion
	s.Mailbox = NewMailboxState()
	s.Event("seed", "", nil) // later events append instead of replacing a nil list
	root := filepath.Join(t.TempDir(), "run")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err = jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if err = EnableJournal(root); err != nil {
		t.Fatalf("managed fixture does not decode: %v", err)
	}
	return root
}

// A writer commits without validating, so a bad commit is only rejected when
// someone reads it. The incremental reader must reject exactly what the full
// reader rejects.
func TestIncrementalReplayRejectsWhatFullValidationRejects(t *testing.T) {
	withVerifyMode(t, journalVerifyDual)
	cases := []struct {
		name   string
		reject bool
		change func(*model.State)
	}{
		{"valid result edit", false, func(s *model.State) { s.Tasks["task_00001"].Result = map[string]string{"answer": "ok", "extra": "1"} }},
		{"valid event", false, func(s *model.State) { s.Event("note", "task_00001", nil) }},
		{"running without a held reservation", true, func(s *model.State) { s.Tasks["task_00001"].Status = "running" }},
		{"reservation attempt beyond the cell", true, func(s *model.State) {
			row := s.Tasks["task_00001"]
			r := s.Execution.Reservations[row.OperationID]
			r.Attempt = 9
			s.Execution.Reservations[row.OperationID] = r
		}},
		{"reservation status invalid", true, func(s *model.State) {
			row := s.Tasks["task_00001"]
			r := s.Execution.Reservations[row.OperationID]
			r.Status = "bogus"
			s.Execution.Reservations[row.OperationID] = r
		}},
		{"cell binding digest changed", true, func(s *model.State) { s.Tasks["task_00001"].BindingDigest = "deadbeef" }},
		{"task specification differs from plan", true, func(s *model.State) { s.Tasks["task_00001"].Spec.Prompt = "tampered" }},
		{"operation id rewritten", true, func(s *model.State) { s.Tasks["task_00001"].OperationID = "op-other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := managedJournalRoot(t)
			got, err, replays := staleRead(t, root, tc.change)
			if err != nil && strings.Contains(err.Error(), "diverged") {
				t.Fatalf("incremental and full validation disagree: %v", err)
			}
			if replays == 0 {
				t.Fatal("expected the warm replay to take the incremental path")
			}
			if tc.reject != (err != nil) {
				t.Fatalf("reject=%v but incremental read error = %v", tc.reject, err)
			}
			want, coldErr := coldRead(t, root)
			if (err != nil) != (coldErr != nil) {
				t.Fatalf("incremental error %v, cold error %v", err, coldErr)
			}
			if err == nil && !reflect.DeepEqual(got, want) {
				t.Fatal("incremental replay and cold decode produced different states")
			}
		})
	}
}

func TestJournalVerifyFullNeverTakesIncrementalPath(t *testing.T) {
	withVerifyMode(t, journalVerifyFull)
	root := journalFixture(t)
	_, err, replays := staleRead(t, root, func(s *model.State) { s.Control = "pause" })
	if err != nil || replays != 0 {
		t.Fatalf("strict mode took the incremental path (replays=%d err=%v)", replays, err)
	}
}

// The incremental path skips marshalling, so the logical size is bounded by the
// journal head instead. The head's bound must really cover the marshalled state.
func TestJournalLogicalBoundCoversMarshalledState(t *testing.T) {
	root := journalFixture(t)
	for i := 0; i < 40; i++ {
		i := i
		if err := Update(root, func(s *model.State) error {
			s.Event("grow", "a", map[string]any{"i": i, "pad": strings.Repeat("x", 50*i)})
			s.Tasks["a"].Notes = map[string]string{"k": strings.Repeat("y", i)}
			if i%7 == 0 {
				s.Tasks["a"].Notes = nil
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		head, _ := journalMarker(t, root)
		s, err := ReadState(root)
		if err != nil {
			t.Fatal(err)
		}
		logical, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(logical)) > head.Storage.LogicalBound {
			t.Fatalf("step %d: marshalled state is %d bytes but LogicalBound is %d", i, len(logical), head.Storage.LogicalBound)
		}
	}
}
