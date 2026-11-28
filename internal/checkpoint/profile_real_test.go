package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

// realRun copies the run named by IRRIGATOR_PROFILE_RUN into a temporary root so
// the benchmarks below can commit to it without touching the original.
func realRun(b *testing.B) string {
	b.Helper()
	src := os.Getenv("IRRIGATOR_PROFILE_RUN")
	if src == "" {
		b.Skip("IRRIGATOR_PROFILE_RUN not set")
	}
	dst := b.TempDir()
	copyTree := func(from, to string) {
		entries, err := os.ReadDir(from)
		if err != nil {
			b.Fatal(err)
		}
		if err := os.MkdirAll(to, 0o700); err != nil {
			b.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(from, e.Name()))
			if err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(to, e.Name()), data, 0o600); err != nil {
				b.Fatal(err)
			}
		}
	}
	copyTree(src, dst)
	copyTree(filepath.Join(src, "checkpoint-data"), filepath.Join(dst, "checkpoint-data"))
	if err := os.WriteFile(filepath.Join(dst, "state.lock"), nil, 0o600); err != nil {
		b.Fatal(err)
	}
	return dst
}

func realFields(s *model.State) map[string]*model.State {
	out := map[string]*model.State{}
	v := reflect.ValueOf(s).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		z := &model.State{}
		reflect.ValueOf(z).Elem().Field(i).Set(v.Field(i))
		out[t.Field(i).Name] = z
	}
	return out
}

// BenchmarkRealSnapshotFields attributes the per-commit whole-state costs to
// each top-level field of a real checkpoint: the deep clone (done twice per
// stale-cache commit), the marshal inside full validation, and a deep equality
// walk of a state against its own clone (the cost class of the diff).
func BenchmarkRealSnapshotFields(b *testing.B) {
	root := realRun(b)
	s, err := FileCheckpointStore{Root: root}.Read(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	for name, part := range realFields(s) {
		part := part
		b.Run("clone/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = cloneCheckpoint(part)
			}
		})
		b.Run("marshal/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := json.Marshal(part); err != nil {
					b.Fatal(err)
				}
			}
		})
		twin := cloneCheckpoint(part)
		b.Run("deepequal/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if !reflect.DeepEqual(part, twin) {
					b.Fatal("clone differs")
				}
			}
		})
	}
}

// BenchmarkRealSnapshotPhases times the pieces of a commit on a real checkpoint.
func BenchmarkRealSnapshotPhases(b *testing.B) {
	root := realRun(b)
	ctx := context.Background()
	store := FileCheckpointStore{Root: root}
	raw, err := readBoundedCheckpoint(filepath.Join(root, "state.json"), StateReadLimit)
	if err != nil {
		b.Fatal(err)
	}
	in, err := loadJournalInputs(root, raw)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("load-files", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := loadJournalInputs(root, raw); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sha256-snapshot", func(b *testing.B) {
		b.SetBytes(int64(len(in.snapshot)))
		for i := 0; i < b.N; i++ {
			_ = digestCheckpoint(in.snapshot)
		}
	})
	s, err := store.Read(ctx)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("unmarshal-snapshot", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var st model.State
			if err := json.Unmarshal(in.snapshot, &st); err != nil {
				b.Fatal(err)
			}
		}
	})
	logical, err := json.Marshal(s)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("full-validate/marshal", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := json.Marshal(s); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("full-validate/decodestate", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := DecodeState(root, logical); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("clone-whole", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = cloneCheckpoint(s)
		}
	})
	b.Run("authority-checks", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := ValidateCampaignAuthority(root, s.Plan); err != nil {
				b.Fatal(err)
			}
			if err := ValidateBillingAuthority(s); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("state-read-cold", func(b *testing.B) {
		b.ReportAllocs()
		key, _ := filepath.Abs(root)
		for i := 0; i < b.N; i++ {
			committedCache.Lock()
			delete(committedCache.entries, key)
			committedCache.Unlock()
			if _, err := store.Read(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("state-read-exact-hit", func(b *testing.B) {
		b.ReportAllocs()
		if _, err := store.Read(ctx); err != nil {
			b.Fatal(err)
		}
		for i := 0; i < b.N; i++ {
			if _, err := store.Read(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	// A stale-cache worker commit, the dominant commit shape: another process
	// has committed since this one's cache entry.
	key, _ := filepath.Abs(root)
	pick := ""
	for id, row := range s.Tasks {
		if row.Status == "pending" && (pick == "" || id < pick) {
			pick = id
		}
	}
	if pick == "" {
		b.Fatal("no pending task to commit against")
	}
	commit := func(i int) {
		if err := store.Update(ctx, CheckpointUpdateOptions{}, func(st *model.State) error {
			st.Tasks[pick].Attempt = i
			st.Event("started", pick, map[string]any{"i": i})
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	commit(1)
	b.Run("commit-stale-cache", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			committedCache.Lock()
			stale := committedCache.entries[key]
			committedCache.Unlock()
			commit(1000 + 2*i)
			commit(1001 + 2*i)
			committedCache.Lock()
			committedCache.entries[key] = stale
			committedCache.Unlock()
			b.StartTimer()
			commit(100000 + i)
		}
	})
}
