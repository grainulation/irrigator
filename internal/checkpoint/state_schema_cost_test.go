package checkpoint

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/model"
)

func TestStateSchemaVersionRefusesFutureVersion(t *testing.T) {
	root := fixture(t, task("a", 0.01))
	statePath := filepath.Join(root, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	// Stamp an unsupported future schema version
	raw["schema_version"] = 999
	futureBytes, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, futureBytes, 0600); err != nil {
		t.Fatal(err)
	}

	_, err = ReadState(root)
	if err == nil {
		t.Fatal("expected error loading state with future schema_version, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported schema_version 999") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestStateSchemaMigrationStampsVersionAndCreatesBackup(t *testing.T) {
	root := fixture(t, task("a", 0.01))
	statePath := filepath.Join(root, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// Remove schema_version to simulate an older legacy checkpoint
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "schema_version")
	legacyBytes, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, legacyBytes, 0600); err != nil {
		t.Fatal(err)
	}

	// Check loaded state has SchemaVersion 0
	loaded, err := ReadState(root)
	if err != nil {
		t.Fatalf("reading legacy state: %v", err)
	}
	if loaded.SchemaVersion != 0 {
		t.Fatalf("expected SchemaVersion 0 for unmigrated legacy state, got %d", loaded.SchemaVersion)
	}

	// Update via fileCheckpointStore triggers migration, backup creation, and schema stamping
	store := FileCheckpointStore{Root: root}
	err = store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Tasks["a"].Status = "verified"
		return nil
	})
	if err != nil {
		t.Fatalf("updating state: %v", err)
	}

	// Verify backup exists and contains pre-migration data
	bakPath := filepath.Join(root, "state.json.bak")
	bakBytes, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatalf("backup file %s not found: %v", bakPath, err)
	}
	if strings.Contains(string(bakBytes), `"schema_version"`) {
		t.Fatalf("backup should contain legacy snapshot without schema_version, got: %s", string(bakBytes))
	}
	if string(bakBytes) != string(legacyBytes) {
		t.Fatal("backup is not a complete copy of the pre-migration checkpoint")
	}

	// Verify state.json is now stamped with CurrentSchemaVersion (2)
	migrated, err := ReadState(root)
	if err != nil {
		t.Fatalf("reading migrated state: %v", err)
	}
	if migrated.SchemaVersion != model.CurrentSchemaVersion {
		t.Fatalf("migrated SchemaVersion = %d, want %d", migrated.SchemaVersion, model.CurrentSchemaVersion)
	}

	// Second update must NOT overwrite state.json.bak
	err = store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Tasks["a"].Status = "failed"
		return nil
	})
	if err != nil {
		t.Fatalf("second update: %v", err)
	}

	bakBytes2, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatalf("reading backup after second update: %v", err)
	}
	if string(bakBytes2) != string(bakBytes) {
		t.Fatal("state.json.bak was overwritten during subsequent update")
	}
}

// A suspect existing backup must stop the migration: the update fails, the
// original checkpoint is byte-identical and unstamped, and the evidence stays.
func TestStateSchemaMigrationBackupFailureLeavesOriginalUnchanged(t *testing.T) {
	cases := map[string]func(t *testing.T, bak string){
		"truncated": func(t *testing.T, bak string) {
			if err := os.WriteFile(bak, []byte(`{"version": 1, "plan_ha`), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"empty": func(t *testing.T, bak string) {
			if err := os.WriteFile(bak, nil, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, bak string) {
			if err := os.Mkdir(bak, 0700); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, bak string) {
			if err := os.Symlink(filepath.Join(filepath.Dir(bak), "state.json"), bak); err != nil {
				t.Fatal(err)
			}
		},
		"unrelated run snapshot": func(t *testing.T, bak string) {
			other := fixture(t, task("zz", 0.01))
			data, err := os.ReadFile(filepath.Join(other, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bak, data, 0600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			root := fixture(t, task("a", 0.01))
			mutateStateJSON(t, root, func(raw map[string]any) { delete(raw, "schema_version") })
			statePath := filepath.Join(root, "state.json")
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			bak := statePath + ".bak"
			plant(t, bak)
			bakBefore, bakErr := os.Lstat(bak)
			if bakErr != nil {
				t.Fatal(bakErr)
			}

			err = FileCheckpointStore{Root: root}.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
				s.Control = "pause"
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "migration backup failed") {
				t.Fatalf("Update error = %v, want migration backup failure", err)
			}
			after, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("original checkpoint changed after a failed migration backup")
			}
			loaded, err := ReadState(root)
			if err != nil || loaded.SchemaVersion != 0 || loaded.Control == "pause" {
				t.Fatalf("checkpoint was migrated or mutated: %v %+v", err, loaded)
			}
			bakAfter, err := os.Lstat(bak)
			if err != nil || bakAfter.Mode() != bakBefore.Mode() {
				t.Fatalf("suspect backup was removed or replaced: %v", err)
			}
		})
	}
}

func TestStateCostMicrosPrecedenceOverUSD(t *testing.T) {
	// When both CostMicros and CostUSD are present, CostMicros strictly wins
	resp := model.WorkResponse{
		CostUSD:    1.50,   // $1.50
		CostMicros: 250000, // $0.25 in micros
	}
	amount := costAmount(resp)
	if amount != 250000 {
		t.Fatalf("costAmount = %d, want 250000 (micros strictly wins)", amount)
	}

	// When only CostUSD is present, converts accurately
	respUSDOnly := model.WorkResponse{
		CostUSD: 0.05,
	}
	if costAmount(respUSDOnly) != 50000 {
		t.Fatalf("costAmount = %d, want 50000", costAmount(respUSDOnly))
	}

	// When CostUSD is NaN or Inf, ResolveCost fails closed
	if _, err := cost.ResolveCost(0, math.NaN()); err == nil {
		t.Fatal("expected error resolving NaN CostUSD")
	}
	if _, err := cost.ResolveCost(0, math.Inf(1)); err == nil {
		t.Fatal("expected error resolving +Inf CostUSD")
	}
	if _, err := cost.ResolveCost(0, -1.0); err == nil {
		t.Fatal("expected error resolving negative CostUSD")
	}
	if _, err := cost.ResolveCost(-100, 0); err == nil {
		t.Fatal("expected error resolving negative CostMicros")
	}
	if _, err := cost.ResolveCost(cost.MaxMicros+1, 0); err == nil {
		t.Fatal("expected error resolving CostMicros exceeding MaxMicros ($1e9)")
	}
}
