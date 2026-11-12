package graph

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func dstatWriteJournal(t *testing.T, root string, rec DeliveryRecord) {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "delivery.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryRevisionBound(t *testing.T) {
	for status, want := range map[string]bool{"accepted": true, "exported": true, "checking": true, "check_failed": true, "running": false, "graph_failed": false, "export_failed": false, "": false} {
		if DeliveryRevisionBound(status) != want {
			t.Fatal("revision bound", status)
		}
	}
}

func TestDeliveryProjectionRunIDAndStatusFences(t *testing.T) {
	complete := &model.State{RunID: "r", Control: "complete", Tasks: map[string]*model.TaskState{"a": {Status: "verified"}}}
	cases := map[string]DeliveryRecord{
		"other run":      {RunID: "other", Status: "accepted", Check: []string{"c"}},
		"no run":         {Status: "accepted", Check: []string{"c"}},
		"unknown status": {RunID: "r", Status: "teleported"},
		"checking empty": {RunID: "r", Status: "checking"},
	}
	for name, rec := range cases {
		root := t.TempDir()
		dstatWriteJournal(t, root, rec)
		view := ProjectDelivery(root, complete)
		dstatWant(t, view, "unknown", "unknown", "inspect")
		_ = name
	}
	root := t.TempDir()
	dstatWriteJournal(t, root, DeliveryRecord{RunID: "r", Status: "accepted", Check: []string{"c"}})
	dstatWant(t, ProjectDelivery(root, complete), "accepted", "passed", "none")
	dstatWant(t, ProjectDelivery(root, &model.State{RunID: "r", Control: "run", Tasks: complete.Tasks}), "stale", "unknown", "inspect")
	dstatWriteJournal(t, root, DeliveryRecord{RunID: "r", Status: "accepted"})
	dstatWant(t, ProjectDelivery(root, complete), "accepted", "not_configured", "none")
}

func TestDeliveryProjectionUncertaintyNeverReadsAsDelivered(t *testing.T) {
	uncertain := &model.State{RunID: "r", Control: "blocked", Tasks: map[string]*model.TaskState{"a": {Status: "uncertain"}, "b": {Status: "verified"}}}
	for _, tc := range []struct{ status, acceptance string }{
		{"running", "pending"}, {"graph_failed", "pending"}, {"export_failed", "pending"}, {"exported", "pending"},
		{"checking", "unknown"}, {"check_failed", "unknown"}, {"accepted", "unknown"},
	} {
		root := t.TempDir()
		dstatWriteJournal(t, root, DeliveryRecord{RunID: "r", Status: tc.status, Check: []string{"check"}})
		dstatWant(t, ProjectDelivery(root, uncertain), "blocked", tc.acceptance, "reconcile_external")
	}
	// Unresolved acceptance-process cleanup is uncertain: never failed, never passed.
	complete := &model.State{RunID: "r", Control: "complete", Tasks: map[string]*model.TaskState{"a": {Status: "verified"}}}
	for _, status := range []string{"check_failed", "checking"} {
		root := t.TempDir()
		dstatWriteJournal(t, root, DeliveryRecord{RunID: "r", Status: status, Check: []string{"check"}})
		raw, _ := json.Marshal(model.ProcessMarker{Status: "running"})
		if err := os.WriteFile(filepath.Join(root, "delivery-check.process.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		dstatWant(t, ProjectDelivery(root, complete), "blocked", "unknown", "inspect")
	}
	// Failed nodes need review; a transient graph failure with all nodes sound resumes locally.
	failedNode := &model.State{RunID: "r", Control: "blocked", Tasks: map[string]*model.TaskState{"a": {Status: "failed"}}}
	root := t.TempDir()
	dstatWriteJournal(t, root, DeliveryRecord{RunID: "r", Status: "graph_failed", Error: "secret /tmp/x"})
	dstatWant(t, ProjectDelivery(root, failedNode), "failed", "not_configured", "inspect")
	dstatWant(t, ProjectDelivery(root, complete), "failed", "not_configured", "resume_local")
}

func TestDeliveryProjectionIsBounded(t *testing.T) {
	complete := &model.State{RunID: "r", Control: "complete", Tasks: map[string]*model.TaskState{"a": {Status: "verified"}}}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "delivery.json"), bytes.Repeat([]byte(" "), DeliveryJournalLimit+1), 0600); err != nil {
		t.Fatal(err)
	}
	dstatWant(t, ProjectDelivery(root, complete), "unknown", "unknown", "inspect")
	if err := os.WriteFile(filepath.Join(root, "delivery.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	dstatWant(t, ProjectDelivery(root, complete), "unknown", "unknown", "inspect")
	linked := t.TempDir()
	outside := filepath.Join(t.TempDir(), "journal.json")
	raw, _ := json.Marshal(DeliveryRecord{RunID: "r", Status: "accepted", Check: []string{"c"}})
	if err := os.WriteFile(outside, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linked, "delivery.json")); err != nil {
		t.Fatal(err)
	}
	dstatWant(t, ProjectDelivery(linked, complete), "unknown", "unknown", "inspect")
	dstatWant(t, ProjectDelivery(t.TempDir(), complete), "not_configured", "not_configured", "none")
}
