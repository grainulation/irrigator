package admission

import (
	"sync"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

// outcomeKind is how one attempt ended.
type outcomeKind int

const (
	outcomeSuccess outcomeKind = iota
	outcomeFailure
	outcomeUncertain
)

// outcome is one finished attempt, seen for the first time.
type outcome struct {
	task     string
	attempt  int
	kind     outcomeKind
	locality locality
	at       time.Time
}

// outcomeObserver is a sensor that learns from finished attempts. The evaluator
// computes the outcomes once per scheduling pass and hands them to every observer
// that wants them, so no sensor re-scans the run.
type outcomeObserver interface {
	observeOutcomes(outcomes []outcome, now time.Time)
}

type seenRow struct {
	attempt int
	status  string
}

// tracker reports each attempt's outcome once, by comparing every task row with
// what it saw last time. The first observation only records the present: attempts
// that finished before this controller started are history, not signal.
type tracker struct {
	mu     sync.Mutex
	seen   map[string]seenRow
	seeded bool
}

// newTracker returns a tracker that has seen nothing.
func newTracker() *tracker { return &tracker{seen: map[string]seenRow{}} }

func kindOf(status string) (outcomeKind, bool) {
	switch status {
	case "verified":
		return outcomeSuccess, true
	case "failed":
		return outcomeFailure, true
	case "uncertain":
		return outcomeUncertain, true
	}
	return 0, false
}

// observe returns the outcomes that appeared since the previous call.
func (t *tracker) observe(s *model.State, now time.Time, locality func(id string) locality) []outcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []outcome
	for id, row := range s.Tasks {
		if row == nil {
			continue
		}
		cur := seenRow{attempt: row.Attempt, status: row.Status}
		prev, known := t.seen[id]
		if known && prev == cur {
			continue // unchanged: no write, no work
		}
		t.seen[id] = cur
		if !t.seeded {
			continue
		}
		if kind, done := kindOf(row.Status); done && row.Attempt > 0 {
			out = append(out, outcome{task: id, attempt: row.Attempt, kind: kind, locality: locality(id), at: now})
		}
	}
	t.seeded = true
	return out
}
