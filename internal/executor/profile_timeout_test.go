package executor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

type timeoutWitness struct {
	t    *testing.T
	want time.Duration
}

func (w timeoutWitness) execute(ctx context.Context, req attemptExecutionRequest) (attemptExecutionReceipt, error) {
	w.t.Helper()
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining > w.want || remaining < w.want-time.Second || req.Work.TimeoutMS != w.want.Milliseconds() {
		w.t.Fatalf("worker/request deadlines disagree: remaining=%v request=%d", remaining, req.Work.TimeoutMS)
	}
	return executionReceipt(req, model.WorkResponse{Output: map[string]string{"answer": "ok"}}, nil), nil
}

func TestProfileTimeoutPinsWorkerAndRequest(t *testing.T) {
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	p := catalog.Profiles["small"]
	p.TimeoutSeconds = 600
	catalog.Profiles["small"] = p
	n := runfixture.Task("a", 0)
	n.Turn = &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}
	plan := model.Plan{APIVersion: launch.ManagedPlanVersion, TurnShape: &model.TurnShape{Roots: 1, MainTurns: 1}, MaxTasks: 1, Tasks: []model.Task{n}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, RunBudgetUSD: "0"}}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(plan, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	if err := workerOperationWithExecutor(root, "a", model.Options{Timeout: time.Second}, "", func(localAttemptExecutor) attemptExecutor {
		return timeoutWitness{t, 600 * time.Second}
	}); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).Tasks["a"].Status != "verified" {
		t.Fatal("timeout witness did not complete")
	}
}

func TestProfileTimeoutBoundsAndAbsentCompatibility(t *testing.T) {
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	p := catalog.Profiles["small"]
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "timeout_seconds") {
		t.Fatal("absent timeout changed legacy binding bytes")
	}
	for _, seconds := range []int{-1, 3601} {
		p.TimeoutSeconds = seconds
		catalog.Profiles["small"] = p
		if launch.ValidateExecutionCatalog(catalog) == nil {
			t.Fatalf("accepted %d seconds", seconds)
		}
	}
	for _, seconds := range []int{0, 1, 600, 3600} {
		p.TimeoutSeconds = seconds
		catalog.Profiles["small"] = p
		if err := launch.ValidateExecutionCatalog(catalog); err != nil {
			t.Fatalf("rejected %d: %v", seconds, err)
		}
	}
}
