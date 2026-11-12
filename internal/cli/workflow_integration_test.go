//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/delivery"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
)

func TestDeliverAcceptanceRecoveryWithoutPaidReplay(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "run")
	dir := filepath.Join(base, "app")
	planPath := filepath.Join(base, "plan.json")
	p := model.Plan{Name: "delivery", Tasks: []model.Task{{ID: "ui", Prompt: "UI", Requires: []string{"files"}, MockResult: map[string]string{"files": `{"index.html":"hello"}`}}}}
	raw, _ := json.Marshal(p)
	if err := os.WriteFile(planPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	opts := model.Options{Workers: 2, Timeout: time.Second, Budget: .1}
	// First acceptance fails. Creating the marker fixes its deterministic input.
	check := `["sh","-c","test -f ready && test -f index.html"]`
	code, err := delivery.Deliver(planPath, root, "mock", dir, "ui", "files", check, 5*time.Second, false, opts)
	if code == 0 || err == nil {
		t.Fatal("accepted failing check")
	}
	// The durable argv preserves user input; PATH preflight must not rewrite it.
	var saved graph.DeliveryRecord
	checkpoint, readErr := os.ReadFile(filepath.Join(root, "delivery.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err = json.Unmarshal(checkpoint, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Check) == 0 || saved.Check[0] != "sh" {
		t.Fatal("rewrote durable bare executable", saved.Check)
	}
	before := read(t, root).Tasks["ui"].Attempt
	code, err = delivery.Deliver(planPath, root, "mock", dir, "ui", "files", check, 5*time.Second, false, opts)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "--recheck") {
		t.Fatal("silently replayed check", code, err)
	}
	if err = os.WriteFile(filepath.Join(dir, "ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	code, err = delivery.Deliver(planPath, root, "mock", dir, "ui", "files", check, 5*time.Second, true, opts)
	if code != 0 || err != nil {
		t.Fatal("recheck", code, err)
	}
	if read(t, root).Tasks["ui"].Attempt != before {
		t.Fatal("replayed model work")
	}
	code, err = delivery.Deliver(planPath, root, "mock", dir, "ui", "files", check, 5*time.Second, false, opts)
	if code != 0 || err != nil {
		t.Fatal("accepted resume", code, err)
	}
	var record graph.DeliveryRecord
	raw, err = os.ReadFile(filepath.Join(root, "delivery.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Status != "accepted" || record.GraphMS <= 0 || record.WallMS <= 0 {
		t.Fatal("missing delivery metrics", record)
	}
}
func TestDeliverDetectsAcceptanceSourceMutation(t *testing.T) {
	base := t.TempDir()
	p := model.Plan{Name: "mutating", Tasks: []model.Task{{ID: "ui", Prompt: "UI", Requires: []string{"files"}, MockResult: map[string]string{"files": `{"index.html":"hello"}`}}}}
	raw, _ := json.Marshal(p)
	plan := filepath.Join(base, "plan.json")
	if err := os.WriteFile(plan, raw, 0600); err != nil {
		t.Fatal(err)
	}
	code, err := delivery.Deliver(plan, filepath.Join(base, "run"), "mock", filepath.Join(base, "app"), "ui", "files", `["/bin/sh","-c","echo changed > index.html"]`, 5*time.Second, false, model.Options{Workers: 1, Timeout: time.Second, Budget: .1})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "modified source") {
		t.Fatal("accepted mutation", code, err)
	}
}

func TestDeliverPreflightDoesNotStartUnknownNode(t *testing.T) {
	base := t.TempDir()
	p := model.Plan{Name: "preflight", Tasks: []model.Task{task("ui", 0)}}
	raw, _ := json.Marshal(p)
	plan := filepath.Join(base, "plan.json")
	if err := os.WriteFile(plan, raw, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "run")
	code, err := delivery.Deliver(plan, root, "mock", filepath.Join(base, "app"), "typo", "files", "", 5*time.Second, false, model.Options{Workers: 1, Timeout: time.Second, Budget: .1})
	if code == 0 || err == nil {
		t.Fatal("accepted typo")
	}
	if read(t, root).Tasks["ui"].Attempt != 0 {
		t.Fatal("ran paid work before rejecting typo")
	}
}
