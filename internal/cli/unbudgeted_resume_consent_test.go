package cli

import (
	"sync"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestUnbudgetedResumeExplicitCallBudgetIsPersistedAndSufficient(t *testing.T) {
	root := unbudgetedLegacyFixture(t, "haiku")
	opts := model.Options{Workers: 1, Timeout: 7 * time.Second, Budget: 0.5}
	if err := admitLegacyResume(root, false, true, opts); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.CallLimits == nil || s.CallLimits.Budget != 0.5 || s.CallLimits.Timeout != 7*time.Second {
		t.Fatalf("explicit budget not persisted: %+v", s.CallLimits)
	}
	if s.UnbudgetedDecision != "" {
		t.Fatalf("a budget is not an allow decision: %q", s.UnbudgetedDecision)
	}
	if executor.UnbudgetedGateApplies(s) || executor.AdmitUnbudgetedExecution(s) != nil {
		t.Fatal("persisted positive budget does not open the gate")
	}
	events := len(s.Events)
	if err := admitLegacyResume(root, false, true, opts); err != nil {
		t.Fatal(err)
	}
	if again := runfixture.Read(t, root); len(again.Events) != events {
		t.Fatal("repeating an admitted budget appended history")
	}
	if s.Tasks["a"].Result["answer"] != "accepted" {
		t.Fatal("accepted result changed")
	}

	// A non-positive budget is never accepted as consent and writes nothing.
	invalid := unbudgetedLegacyFixture(t, "haiku")
	if err := admitLegacyResume(invalid, false, true, model.Options{Workers: 1, Timeout: time.Second, Budget: 0}); err == nil {
		t.Fatal("zero budget accepted as an explicit budget")
	}
	if got := runfixture.Read(t, invalid); got.SchemaVersion != 0 || got.CallLimits != nil || got.UnbudgetedDecision != "" {
		t.Fatalf("invalid budget mutated the checkpoint: %+v", got)
	}
}

func TestUnbudgetedResumeConcurrentAllowRecordsExactlyOnce(t *testing.T) {
	root := unbudgetedLegacyFixture(t, "haiku")
	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = admitLegacyResume(root, true, false, runfixture.UnbudgetedTestOptions())
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	s := runfixture.Read(t, root)
	if got := unbudgetedCountEvents(s, "legacy_unbudgeted_allowed"); got != 1 || s.UnbudgetedDecision != checkpoint.UnbudgetedAllow {
		t.Fatalf("allow recorded %d times (decision %q), want exactly once", got, s.UnbudgetedDecision)
	}
}
