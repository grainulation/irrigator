package checkpoint

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestCloneSharingPlanSharesOnlyThePlan(t *testing.T) {
	root := workerPatternRun(t, "managed", 50)
	s, err := FileCheckpointStore{Root: root}.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := cloneSharingPlan(s)
	if !reflect.DeepEqual(c, s) {
		t.Fatal("the copy differs from the original")
	}
	if &c.Plan.Tasks[0] != &s.Plan.Tasks[0] {
		t.Fatal("the plan's tasks should be shared, not duplicated")
	}
	full := cloneCheckpoint(s)
	if &full.Plan.Tasks[0] == &s.Plan.Tasks[0] {
		t.Fatal("cloneCheckpoint must stay a deep copy")
	}
	// Everything but the plan is private to the copy.
	c.Tasks["task_00001"].Attempt = 99
	if s.Tasks["task_00001"].Attempt == 99 {
		t.Fatal("task rows are shared")
	}
	c.Events = append(c.Events, model.Event{Kind: "x"})
	if len(s.Events) == len(c.Events) {
		t.Fatal("events are shared")
	}
}

func TestCommitsLeaveThePlanAloneAndPassTheDualVerifier(t *testing.T) {
	old := journalVerifyMode
	journalVerifyMode = journalVerifyDual
	t.Cleanup(func() { journalVerifyMode = old })
	root := workerPatternRun(t, "managed", 50)
	store := FileCheckpointStore{Root: root}
	for i := 1; i <= 6; i++ {
		if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
			s.Tasks["task_00001"].Attempt = i
			s.Event("started", "task_00001", map[string]any{"i": i})
			return nil
		}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
}

func TestACallbackThatWritesThroughThePlanIsRefused(t *testing.T) {
	old := journalVerifyMode
	journalVerifyMode = journalVerifyDual
	t.Cleanup(func() { journalVerifyMode = old })
	root := workerPatternRun(t, "managed", 50)
	store := FileCheckpointStore{Root: root}
	// The first commit warms the cache so the next one starts from a shared plan.
	if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Tasks["task_00001"].Attempt = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*model.State)
	}{
		{"task id", func(s *model.State) { s.Plan.Tasks[0].ID = "tampered" }},
		{"task prompt", func(s *model.State) { s.Plan.Tasks[3].Prompt += " tampered" }},
		{"task requires", func(s *model.State) { s.Plan.Tasks[5].Requires = append(s.Plan.Tasks[5].Requires[:0], "tampered") }},
	}
	for _, c := range cases {
		err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
			c.mutate(s)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "shared plan") {
			t.Fatalf("%s: expected the shared-plan refusal, got %v", c.name, err)
		}
	}
	// A refused commit must not leave the changed plan in the cache.
	key, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	committedCache.Lock()
	_, cached := committedCache.entries[key]
	committedCache.Unlock()
	if cached {
		t.Fatal("the cache entry was kept after a refused write to the shared plan")
	}
	fresh, err := store.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Plan.Tasks[0].ID == "tampered" || strings.Contains(fresh.Plan.Tasks[3].Prompt, "tampered") {
		t.Fatal("a later read saw the refused write")
	}
	if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Tasks["task_00001"].Attempt = 2
		return nil
	}); err != nil {
		t.Fatalf("a commit after the refusals failed: %v", err)
	}
}

func TestCloneSharingPlanCopiesEventStructsAndSharesTheirDetails(t *testing.T) {
	root := workerPatternRun(t, "managed", 20)
	store := FileCheckpointStore{Root: root}
	if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Event("one", "task_00001", map[string]any{"k": "v"})
		s.Event("two", "task_00002", map[string]any{"k": "w"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := cloneSharingPlan(s)
	if !reflect.DeepEqual(c, s) {
		t.Fatal("the copy differs from the original")
	}
	last := len(s.Events) - 1
	if &c.Events[0] == &s.Events[0] {
		t.Fatal("event structs must be copied into a private array")
	}
	// Replacing or appending in the copy never reaches the original.
	c.Events[last].Kind = "changed"
	c.Events = append(c.Events, model.Event{Kind: "extra"})
	if s.Events[last].Kind == "changed" || len(s.Events) == len(c.Events) {
		t.Fatal("the copy's events alias the original's array")
	}
	// The detail values are the shared part.
	d1, ok1 := s.Events[last-1].Detail.(map[string]any)
	d2, ok2 := c.Events[last-1].Detail.(map[string]any)
	if !ok1 || !ok2 || reflect.ValueOf(d1).Pointer() != reflect.ValueOf(d2).Pointer() {
		t.Fatal("event details should be shared")
	}
	// A nil event list stays nil.
	z := cloneSharingPlan(&model.State{})
	if z.Events != nil {
		t.Fatal("nil events became non-nil")
	}
}

func TestACallbackThatEditsAnExistingEventDetailIsRefusedAndEvicted(t *testing.T) {
	old := journalVerifyMode
	journalVerifyMode = journalVerifyDual
	t.Cleanup(func() { journalVerifyMode = old })
	root := workerPatternRun(t, "managed", 20)
	store := FileCheckpointStore{Root: root}
	if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Event("one", "task_00001", map[string]any{"k": "v"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Warm the cache so the next update starts from shared details.
	if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Tasks["task_00001"].Attempt = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		for i := range s.Events {
			if d, ok := s.Events[i].Detail.(map[string]any); ok && d["k"] == "v" {
				d["k"] = "tampered"
			}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "event's detail") {
		t.Fatalf("expected the refusal, got %v", err)
	}
	key, _ := filepath.Abs(root)
	committedCache.Lock()
	_, cached := committedCache.entries[key]
	committedCache.Unlock()
	if cached {
		t.Fatal("the cache entry was kept after a refused write to an event detail")
	}
	fresh, err := store.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range fresh.Events {
		if d, ok := e.Detail.(map[string]any); ok && d["k"] == "tampered" {
			t.Fatal("a later read saw the refused write")
		}
	}
	// New events appended with their own details commit normally.
	if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Event("three", "task_00001", map[string]any{"k": "new"})
		return nil
	}); err != nil {
		t.Fatalf("appending after a refusal failed: %v", err)
	}
}

func TestAWriteThroughThePlanIsRefusedEvenWhenTheContextEndsFirst(t *testing.T) {
	old := journalVerifyMode
	journalVerifyMode = journalVerifyDual
	t.Cleanup(func() { journalVerifyMode = old })
	root := workerPatternRun(t, "managed", 20)
	store := FileCheckpointStore{Root: root}
	if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Tasks["task_00001"].Attempt = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := store.Update(ctx, CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Plan.Tasks[0].Prompt += " tampered"
		cancel()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "shared plan") {
		t.Fatalf("expected the shared-plan refusal ahead of the context error, got %v", err)
	}
	key, _ := filepath.Abs(root)
	committedCache.Lock()
	_, cached := committedCache.entries[key]
	committedCache.Unlock()
	if cached {
		t.Fatal("the poisoned cache entry was kept")
	}
}
