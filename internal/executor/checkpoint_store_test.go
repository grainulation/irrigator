package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Independent adapter: bytes are the committed record; callbacks never see it.
type memoryCheckpointStore struct {
	mu   sync.Mutex
	data []byte
}

func (m *memoryCheckpointStore) Read(ctx context.Context) (*model.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var s model.State
	err := json.Unmarshal(m.data, &s)
	return &s, err
}
func (m *memoryCheckpointStore) Update(ctx context.Context, o checkpoint.CheckpointUpdateOptions, fn func(*model.State) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.Nonblocking {
		if !m.mu.TryLock() {
			return errors.New("busy")
		}
	} else {
		m.mu.Lock()
	}
	defer m.mu.Unlock()
	var s model.State
	if err := json.Unmarshal(m.data, &s); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(&s); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(&s)
	if err == nil {
		m.data = data
	}
	return err
}
func testCheckpointAdapters(t *testing.T, fn func(*testing.T, checkpoint.CheckpointStore)) {
	t.Helper()
	for _, name := range []string{"file", "memory"} {
		t.Run(name, func(t *testing.T) {
			root := runfixture.New(t, runfixture.Task("cell", 0))
			var store checkpoint.CheckpointStore = checkpoint.FileCheckpointStore{Root: root}
			if name == "memory" {
				s, err := store.Read(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(s)
				store = &memoryCheckpointStore{data: data}
			}
			fn(t, store)
		})
	}
}
func TestCheckpointStoreConformance(t *testing.T) {
	testCheckpointAdapters(t, func(t *testing.T, store checkpoint.CheckpointStore) {
		initial, err := store.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		initial.Control = "mutated"
		again, _ := store.Read(context.Background())
		if again.Control == "mutated" {
			t.Fatal("read aliases store")
		}
		calls := 0
		sentinel := errors.New("abort")
		if err := store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error { calls++; s.Control = "mutated"; return sentinel }); !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		again, _ = store.Read(context.Background())
		if calls != 1 || again.Control == "mutated" {
			t.Fatal("callback abort or exactly-once violated")
		}
		before := len(again.Events)
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error { s.Event("conformance", "", nil); return nil }); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		after, _ := store.Read(context.Background())
		if len(after.Events) != before+12 {
			t.Fatal("lost update")
		}
	})
}
func TestCheckpointCellsAdapterFence(t *testing.T) {
	testCheckpointAdapters(t, func(t *testing.T, store checkpoint.CheckpointStore) {
		if err := store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error { s.Control = "run"; return nil }); err != nil {
			t.Fatal(err)
		}
		s, _ := store.Read(context.Background())
		cells := transactionalCells{store: store}
		a, err := cells.beginAttempt("cell", s.Tasks["cell"].OperationID, 1)
		if err != nil {
			t.Fatal(err)
		}
		stale := a.Fence
		stale.Attempt++
		if err := cells.recordOutcome(managed.CellOutcome{Fence: stale}); err == nil {
			t.Fatal("stale fence accepted")
		}
		before, _ := store.Read(context.Background())
		if before.Tasks["cell"].Attempt != a.Fence.Attempt {
			t.Fatal("stale commit mutated attempt")
		}
	})
}
func TestCheckpointStoreNonblocking(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("cell", 0))
	lock, err := checkpoint.FileLock(filepath.Join(root, "state.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(lock)
	called := false
	err = (checkpoint.FileCheckpointStore{Root: root}).Update(context.Background(), checkpoint.CheckpointUpdateOptions{Nonblocking: true}, func(*model.State) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("busy transaction admitted callback")
	}
}

func TestCheckpointStoreManagedAtomicAdmission(t *testing.T) {
	for _, name := range []string{"file", "memory"} {
		t.Run(name, func(t *testing.T) {
			root := runfixture.ManagedStateFixture(t, "claude_cli", "0.25", 2)
			var store checkpoint.CheckpointStore = checkpoint.FileCheckpointStore{Root: root}
			if name == "memory" {
				s, _ := store.Read(context.Background())
				data, _ := json.Marshal(s)
				store = &memoryCheckpointStore{data: data}
			}
			cells := transactionalCells{store: store}
			var wg sync.WaitGroup
			winners := make(chan AdmittedCellAttempt, 2)
			for _, id := range []string{"a", "b"} {
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					a, e := cells.beginAttempt(id, "", 123)
					if e == nil {
						winners <- a
					} else if !errors.Is(e, launch.ErrNoWork) {
						t.Error(e)
					}
				}(id)
			}
			wg.Wait()
			close(winners)
			var admitted []AdmittedCellAttempt
			for a := range winners {
				admitted = append(admitted, a)
			}
			if len(admitted) != 1 {
				t.Fatalf("admitted %d", len(admitted))
			}
			a := admitted[0]
			if e := cells.recordOutcome(managed.CellOutcome{Fence: a.Fence, Cost: .1, Err: &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: "test ambiguity"}}); e != nil {
				t.Fatal(e)
			}
			s, _ := store.Read(context.Background())
			used, e := checkpoint.ManagedBudgetUsed(s.Execution)
			if e != nil || used != 250000 {
				t.Fatalf("unknown reservation lost: %d %v", used, e)
			}
			data, _ := json.Marshal(s)
			if e := cells.recordOutcome(managed.CellOutcome{Fence: a.Fence, Cost: .01}); e == nil {
				t.Fatal("replayed outcome accepted")
			}
			after, _ := store.Read(context.Background())
			got, _ := json.Marshal(after)
			if string(data) != string(got) {
				t.Fatal("replay changed ledger")
			}
		})
	}
}

func BenchmarkCheckpointStore(b *testing.B) {
	for _, operation := range []string{"Read", "Update"} {
		b.Run(operation, func(b *testing.B) {
			tasks := make([]model.Task, 100)
			for i := range tasks {
				tasks[i] = runfixture.Task(fmt.Sprintf("cell%d", i), 0)
			}
			root := filepath.Join(b.TempDir(), "run")
			if e := checkpoint.Initialize(model.Plan{Name: "benchmark", Tasks: tasks}, root, "mock"); e != nil {
				b.Fatal(e)
			}
			store := checkpoint.FileCheckpointStore{Root: root}
			if e := store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error {
				for i := 0; i < 100; i++ {
					s.Event("benchmark", "", nil)
				}
				return nil
			}); e != nil {
				b.Fatal(e)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var e error
				if operation == "Read" {
					_, e = store.Read(context.Background())
				} else {
					e = store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error { s.Control = "pause"; return nil })
				}
				if e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}

func TestCheckpointStoreCorruptSnapshotRejectsCallback(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("cell", 0))
	store := checkpoint.FileCheckpointStore{Root: root}
	s, e := store.Read(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	s.PlanHash = "corrupt"
	if e := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); e != nil {
		t.Fatal(e)
	}
	called := false
	if e := store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(*model.State) error { called = true; return nil }); e == nil || called {
		t.Fatal("corrupt checkpoint reached callback")
	}
}

func TestCheckpointStorePublicationFailureDoesNotRetry(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("cell", 0))
	store := checkpoint.FileCheckpointStore{Root: root}
	calls := 0
	// Change the destination only after the validated snapshot is in the callback.
	// A rename onto a directory must fail, independently of process privileges.
	e := store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error {
		calls++
		if e := os.Remove(filepath.Join(root, "state.json")); e != nil {
			return e
		}
		return os.Mkdir(filepath.Join(root, "state.json"), 0700)
	})
	if e == nil || calls != 1 {
		t.Fatalf("publication failure retried or acknowledged: calls=%d err=%v", calls, e)
	}
}

func TestCheckpointStoreCancellation(t *testing.T) {
	testCheckpointAdapters(t, func(t *testing.T, store checkpoint.CheckpointStore) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, e := store.Read(ctx); !errors.Is(e, context.Canceled) {
			t.Fatalf("cancelled read: %v", e)
		}
		called := false
		if e := store.Update(ctx, checkpoint.CheckpointUpdateOptions{}, func(*model.State) error { called = true; return nil }); !errors.Is(e, context.Canceled) || called {
			t.Fatalf("cancelled update: called=%v err=%v", called, e)
		}
		before, e := store.Read(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		original, _ := json.Marshal(before)
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		e = store.Update(ctx, checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error { s.Control = "changed"; cancel(); return nil })
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("cancel before commit: %v", e)
		}
		after, e := store.Read(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		got, _ := json.Marshal(after)
		if string(got) != string(original) {
			t.Fatal("cancelled transaction published")
		}
	})
}
