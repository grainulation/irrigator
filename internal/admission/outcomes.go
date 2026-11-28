package admission

import (
	"sync"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

// OutcomeKind is how one attempt ended.
type OutcomeKind int

const (
	OutcomeSuccess OutcomeKind = iota
	OutcomeFailure
	OutcomeUncertain
)

// Outcome is one finished attempt, seen for the first time.
type Outcome struct {
	Task     string
	Attempt  int
	Kind     OutcomeKind
	Locality Locality
	At       time.Time
}

// OutcomeObserver is a sensor that learns from finished attempts. The evaluator
// computes the outcomes once per scheduling pass and hands them to every observer
// that wants them, so no sensor re-scans the run.
type OutcomeObserver interface {
	ObserveOutcomes(outcomes []Outcome, now time.Time)
}

type seenRow struct {
	attempt int
	status  string
}

// Tracker reports each attempt's outcome once, by comparing every task row with
// what it saw last time. The first observation only records the present: attempts
// that finished before this controller started are history, not signal.
type Tracker struct {
	mu     sync.Mutex
	seen   map[string]seenRow
	seeded bool
}

// NewTracker returns a tracker that has seen nothing.
func NewTracker() *Tracker { return &Tracker{seen: map[string]seenRow{}} }

func kindOf(status string) (OutcomeKind, bool) {
	switch status {
	case "verified":
		return OutcomeSuccess, true
	case "failed":
		return OutcomeFailure, true
	case "uncertain":
		return OutcomeUncertain, true
	}
	return 0, false
}

// Observe returns the outcomes that appeared since the previous call.
func (t *Tracker) Observe(s *model.State, now time.Time, locality func(id string) Locality) []Outcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Outcome
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
			out = append(out, Outcome{Task: id, Attempt: row.Attempt, Kind: kind, Locality: locality(id), At: now})
		}
	}
	t.seeded = true
	return out
}
