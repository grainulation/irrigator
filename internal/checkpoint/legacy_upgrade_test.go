package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

// This fixed input is synthetic, not a captured historical run. Unlike stripping
// one field from today's initializer, it pins multiple absent legacy fields and
// an accepted result alongside an uncertain dependent attempt.
func legacyUpgradeFixture(t *testing.T) (string, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "legacy-pre-schema", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return root, data
}

func TestLegacyFixtureUpgradePreservesEvidenceAndConsent(t *testing.T) {
	root, original := legacyUpgradeFixture(t)
	var wire model.State
	if err := json.Unmarshal(original, &wire); err != nil {
		t.Fatal(err)
	}
	before, err := ReadState(root)
	if err != nil {
		t.Fatalf("load fixed legacy checkpoint: %v", err)
	}
	if before.SchemaVersion != 0 || before.RunID != "legacy-"+wire.PlanHash {
		t.Fatalf("unexpected legacy identity: schema=%d run=%q", before.SchemaVersion, before.RunID)
	}
	if !reflect.DeepEqual(before.Plan, wire.Plan) || before.PlanHash != wire.PlanHash || !reflect.DeepEqual(before.Events, wire.Events) {
		t.Fatal("loading changed admitted plan, plan hash or historical events")
	}
	if row := before.Tasks["draft"]; row.Status != "verified" || row.Attempt != 2 || row.Result["brief"] != "accepted fixture brief" || row.ReportedCostMicros != 125000 {
		t.Fatalf("accepted legacy evidence was not preserved: %+v", row)
	}
	if row := before.Tasks["publish"]; row.Status != "uncertain" || row.Attempt != 1 || row.Error != wire.Tasks["publish"].Error {
		t.Fatalf("uncertain legacy evidence was not preserved: %+v", row)
	}
	for id, row := range before.Tasks {
		if row.Generation != 1 || row.OperationID != model.OperationID(before.RunID, id, 1) {
			t.Fatalf("legacy identity was not deterministically derived for %s", id)
		}
	}
	readBytes, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil || !bytes.Equal(readBytes, original) {
		t.Fatal("read rewrote the original checkpoint")
	}
	store := FileCheckpointStore{Root: root}
	for update := range 2 {
		if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(*model.State) error { return nil }); err != nil {
			t.Fatalf("migration update %d: %v", update, err)
		}
		backup, err := os.ReadFile(filepath.Join(root, "state.json.bak"))
		if err != nil || !bytes.Equal(backup, original) {
			t.Fatalf("update %d did not preserve exact original backup: %v", update, err)
		}
		after, err := ReadState(root)
		if err != nil {
			t.Fatal(err)
		}
		if after.SchemaVersion != model.CurrentSchemaVersion {
			t.Fatalf("migration did not stamp supported schema: %d", after.SchemaVersion)
		}
		if after.CallLimits != nil || after.UnbudgetedDecision != "" {
			t.Fatal("migration invented budget limits or operator consent")
		}
		want := *before
		want.SchemaVersion = model.CurrentSchemaVersion
		if !reflect.DeepEqual(after, &want) {
			t.Fatal("migration changed state beyond the storage schema stamp")
		}
	}
}

func TestLegacyFixtureFutureVersionsFailWithoutMutation(t *testing.T) {
	for _, field := range []string{"schema_version", "version"} {
		t.Run(field, func(t *testing.T) {
			root, original := legacyUpgradeFixture(t)
			var wire map[string]any
			if err := json.Unmarshal(original, &wire); err != nil {
				t.Fatal(err)
			}
			wire[field] = 999
			future, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "state.json")
			if err := os.WriteFile(path, future, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadState(root); err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("future checkpoint read: %v", err)
			}
			called := false
			err = Update(root, func(*model.State) error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("future checkpoint invoked reducer or was accepted: called=%v err=%v", called, err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, future) {
				t.Fatal("future checkpoint was rewritten")
			}
			if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
				t.Fatalf("future checkpoint unexpectedly migrated: %v", err)
			}
		})
	}
}
