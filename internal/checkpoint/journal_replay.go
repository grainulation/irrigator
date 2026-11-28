package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/grainulation/irrigator/internal/model"
)

const (
	journalVerifyFull = "full" // always marshal and re-parse the whole state
	journalVerifyDual = "dual" // run both paths and fail on any difference
)

// journalVerifyMode selects how a warm journal replay is validated. The default
// validates the replayed typed state directly; "full" restores the whole-state
// marshal and re-parse, and "dual" runs both and fails if they disagree.
var journalVerifyMode = os.Getenv("IRRIGATOR_JOURNAL_VERIFY")

var errLogicalTooLarge = errors.New("logical checkpoint exceeds byte limit")

func errUnsupportedSchema(version int) error {
	return fmt.Errorf("unsupported schema_version %d (max supported %d)", version, model.CurrentSchemaVersion)
}

// replayTail records what a warm replay applied on top of the cached state.
type replayTail struct {
	patches     []statePatch
	eventRanges [][2]int
}

func (t *replayTail) record(p statePatch, eventsBefore, eventsAfter int) {
	t.patches = append(t.patches, statePatch{Path: p.Path, Append: p.Append, Remove: p.Remove})
	if len(p.Path) > 0 && p.Path[0] == "events" && eventsAfter > eventsBefore {
		t.eventRanges = append(t.eventRanges, [2]int{eventsBefore, eventsAfter})
	}
}

// validateFullReplay is the original validation of a replayed state: marshal it,
// bound its size, and decode the bytes with every check. It returns the logical
// size so the caller can keep its caching decision.
func validateFullReplay(root string, s *model.State) (*model.State, int, error) {
	logical, err := json.Marshal(s)
	if err != nil {
		return nil, 0, err
	}
	if int64(len(logical)) > stateReadLimit {
		return nil, 0, errLogicalTooLarge
	}
	validated, err := DecodeState(root, logical)
	if err != nil {
		return nil, 0, err
	}
	return validated, len(logical), nil
}

// validateIncrementalReplay validates a warm replay without re-serializing the
// whole state. It reports handled=false when the replay has a shape it does not
// cover, leaving the caller to take the full path.
//
// This is equivalent to validateFullReplay because the full path re-parses bytes
// it just produced from the typed state: strict-JSON and field-name checks can
// only see canonical output, the size is bounded by the journal head's
// LogicalBound (checked against the real snapshot and log sizes by the caller),
// and marshal then unmarshal is idempotent once the touched nodes are normalized
// the way that round trip would normalize them. The typed validators and
// normalization are the same ones DecodeState runs.
func validateIncrementalReplay(root string, s *model.State, tail *replayTail) (*model.State, bool, error) {
	if !canonicalizeReplay(s, tail) {
		return nil, false, nil
	}
	if s.SchemaVersion > model.CurrentSchemaVersion {
		return nil, true, errUnsupportedSchema(s.SchemaVersion)
	}
	checkpointIncrementalReplays.Add(1)
	validated, err := validateDecodedState(root, s)
	return validated, true, err
}

var scalarStateFields = sync.OnceValue(func() map[string]bool {
	fields := map[string]bool{}
	t := reflect.TypeOf(model.State{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		switch f.Type.Kind() {
		case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			fields[name] = true
		}
	}
	return fields
})

// canonicalizeReplay normalizes the nodes a replay touched exactly as a marshal
// and re-parse of the whole state would, and reports false for any patch shape it
// does not recognize: removals, whole-collection replacement, and anything that
// is not a task row, an appended event, a reservation or a scalar state field.
func canonicalizeReplay(s *model.State, tail *replayTail) bool {
	tasks := map[string]bool{}
	reservations := map[string]bool{}
	for _, p := range tail.patches {
		if p.Remove || len(p.Path) == 0 {
			return false
		}
		switch p.Path[0] {
		case "tasks":
			if len(p.Path) < 2 {
				return false
			}
			tasks[p.Path[1]] = true
		case "events":
			if len(p.Path) != 1 || !p.Append {
				return false
			}
		case "execution":
			if len(p.Path) < 3 || p.Path[1] != "reservations" {
				return false
			}
			reservations[p.Path[2]] = true
		default:
			if len(p.Path) != 1 || !scalarStateFields()[p.Path[0]] {
				return false
			}
		}
	}
	for id := range tasks {
		row := s.Tasks[id]
		if row == nil {
			continue
		}
		var normalized model.TaskState
		if !roundTrip(row, &normalized) {
			return false
		}
		s.Tasks[id] = &normalized
	}
	for _, r := range tail.eventRanges {
		for i := r[0]; i < r[1] && i < len(s.Events); i++ {
			var normalized model.Event
			if !roundTrip(s.Events[i], &normalized) {
				return false
			}
			s.Events[i] = normalized
		}
	}
	if len(reservations) > 0 {
		if s.Execution == nil {
			return false
		}
		for op := range reservations {
			r, present := s.Execution.Reservations[op]
			if !present {
				continue
			}
			var normalized model.ManagedReservation
			if !roundTrip(r, &normalized) {
				return false
			}
			s.Execution.Reservations[op] = normalized
		}
	}
	return true
}

// roundTrip marshals v and parses it into out, reporting whether both worked.
func roundTrip(v, out any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, out) == nil
}

// snapshotShared deep-copies the data cloneSharingPlan shares and returns a check
// that it is still equal to the live data. A deep copy is needed because the live
// copies share pointers, so comparing two shared copies could never see a write.
func snapshotShared(s *model.State) func() bool {
	snap := cloneCheckpoint(&model.State{Plan: s.Plan, Events: s.Events})
	return func() bool {
		return reflect.DeepEqual(snap.Plan, s.Plan) && reflect.DeepEqual(snap.Events, s.Events)
	}
}

// guardSharedState returns a check to run after the callback that the plan and
// event details the state shares with the commit cache were not written through.
// It costs a deep copy of both, so it runs only in the dual verify mode;
// otherwise it does nothing.
func guardSharedState(root string, base *model.State) func() error {
	if journalVerifyMode != journalVerifyDual || base == nil {
		return func() error { return nil }
	}
	same := snapshotShared(base)
	return func() error {
		if same() {
			return nil
		}
		// The write already reached the cached copy: drop it so no later commit
		// in this process starts from the changed data.
		if key, err := filepath.Abs(root); err == nil {
			committedCache.Lock()
			delete(committedCache.entries, key)
			committedCache.Unlock()
		}
		return errors.New("checkpoint callback mutated the shared plan or an existing event's detail; both are immutable once committed")
	}
}
