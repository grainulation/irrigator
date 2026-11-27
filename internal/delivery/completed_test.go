package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func completedFixture(t *testing.T) (string, AssembleRequest) {
	t.Helper()
	task := runfixture.Task("source", 0)
	task.Requires = []string{"files"}
	root := runfixture.New(t, task)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := json.Marshal(map[string]string{"source.txt": "accepted"})
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "complete"
		s.Tasks["source"].Status = "verified"
		s.Tasks["source"].Result = map[string]string{"files": string(files)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	return root, AssembleRequest{CompletedRequest: CompletedRequest{RequestID: "assembly-one", ExpectedRunID: s.RunID, ExpectedPlanHash: s.PlanHash, ExpectedRevision: s.GraphRevision}, Directory: filepath.Join(t.TempDir(), "candidate"), Nodes: []string{"source"}, Field: "files"}
}

func TestCompletedAssemblyNeverStartsGraphAndRequiresItsIdentity(t *testing.T) {
	root, in := completedFixture(t)
	if err := checkpoint.Update(root, func(s *model.State) error { s.Control = "pause"; s.Tasks["source"].Status = "pending"; return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if _, err := AssembleCompletedRun(context.Background(), root, in); err == nil {
		t.Fatal("incomplete graph assembled")
	}
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("assembly started or changed graph")
	}
	if _, err := os.Stat(in.Directory); !os.IsNotExist(err) {
		t.Fatal("candidate published")
	}
}

func TestCompletedAssemblyMigratesOnlyAfterValidPreconditions(t *testing.T) {
	root, in := completedFixture(t)
	before, _ := os.ReadFile(filepath.Join(root, "state.json"))
	stale := in
	stale.ExpectedRunID = "another-run"
	if _, err := AssembleCompletedRun(context.Background(), root, stale); err == nil {
		t.Fatal("stale run accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("invalid request migrated checkpoint")
	}
	if _, err := AssembleCompletedRun(context.Background(), root, in); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(filepath.Join(root, "state.json"))
	var head struct {
		Schema  int             `json:"schema_version"`
		Storage json.RawMessage `json:"checkpoint_storage"`
	}
	if err := json.Unmarshal(after, &head); err != nil || head.Schema != 3 || len(head.Storage) == 0 {
		t.Fatal("old writers were not fenced", string(after), err)
	}
}

func TestCompletedAssemblyAndExplicitCheckAreRetrySafe(t *testing.T) {
	root, in := completedFixture(t)
	assembled, err := AssembleCompletedRun(context.Background(), root, in)
	if err != nil || assembled.Status != "exported" || assembled.CheckStarted || assembled.Dispatched {
		t.Fatal(assembled, err)
	}
	summary, err := Summarize(root)
	if err != nil || summary.CandidateSHA256 == nil || *summary.CandidateSHA256 != assembled.CandidateSHA256 {
		t.Fatal("candidate summary identity differs", summary, err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "delivery.json"))
	again, err := AssembleCompletedRun(context.Background(), root, in)
	if err != nil || !again.Replayed {
		t.Fatal(again, err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "delivery.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("replayed assembly changed journal")
	}
	check := CheckRequest{CompletedRequest: in.CompletedRequest, ExpectedCandidateSHA256: assembled.CandidateSHA256, Check: []string{"sh", "-c", "echo ran >> check-count"}, Timeout: 5 * time.Second}
	check.RequestID = "check-one"
	result, err := CheckCompletedDelivery(context.Background(), root, check)
	if err != nil || result.Status != "accepted" || !result.CheckStarted || result.Dispatched {
		t.Fatal(result, err)
	}
	result, err = CheckCompletedDelivery(context.Background(), root, check)
	if err != nil || !result.Replayed {
		t.Fatal(result, err)
	}
	count, err := os.ReadFile(filepath.Join(in.Directory, "check-count"))
	if err != nil || string(count) != "ran\n" {
		t.Fatal("checker repeated", string(count), err)
	}
	check.RequestID = "check-new"
	if _, err := CheckCompletedDelivery(context.Background(), root, check); err == nil {
		t.Fatal("accepted checker repeated without recheck")
	}
	check.Recheck = true
	if result, err = CheckCompletedDelivery(context.Background(), root, check); err != nil || result.Status != "accepted" {
		t.Fatal(result, err)
	}
	count, _ = os.ReadFile(filepath.Join(in.Directory, "check-count"))
	if string(count) != "ran\nran\n" {
		t.Fatal(string(count))
	}
}

func TestCompletedCheckRefusesStaleCandidateAndDoesNotReplayInterruptedIntent(t *testing.T) {
	root, in := completedFixture(t)
	assembled, err := AssembleCompletedRun(context.Background(), root, in)
	if err != nil {
		t.Fatal(err)
	}
	check := CheckRequest{CompletedRequest: in.CompletedRequest, ExpectedCandidateSHA256: assembled.CandidateSHA256, Check: []string{"sh", "-c", "echo ran >> count"}, Timeout: 5 * time.Second}
	check.RequestID = "interrupted"
	rec, err := readDelivery(root)
	if err != nil {
		t.Fatal(err)
	}
	rec.Check = check.Check
	rec.Status = "checking"
	rec.CheckPrefix = "delivery-check-" + requestDigest("check", check)[:32]
	rec.Requests[check.RequestID] = graph.DeliveryRequestReceipt{Digest: requestDigest("check", check), Kind: "check", Status: "checking", CandidateSHA256: assembled.CandidateSHA256, CheckStarted: true}
	if err = jsonio.AtomicJSON(filepath.Join(root, "delivery.json"), rec); err != nil {
		t.Fatal(err)
	}
	if view := graph.ProjectDelivery(root, runfixture.Read(t, root)); view.Stage != "blocked" || view.Acceptance != "unknown" {
		t.Fatal("missing marker was treated as cleanup evidence", view)
	}
	result, err := CheckCompletedDelivery(context.Background(), root, check)
	if err != nil || !result.Replayed || result.Status != "unknown" {
		t.Fatal(result, err)
	}
	if _, err := os.Stat(filepath.Join(in.Directory, "count")); !os.IsNotExist(err) {
		t.Fatal("interrupted check replayed")
	}
	check.RequestID = "fresh-check"
	check.Recheck = true
	if _, err := CheckCompletedDelivery(context.Background(), root, check); err == nil {
		t.Fatal("recheck inferred cleanup from a missing process marker")
	}
	check.RequestID = "new-check"
	check.ExpectedCandidateSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	check.Recheck = true
	if _, err := CheckCompletedDelivery(context.Background(), root, check); err == nil {
		t.Fatal("stale candidate accepted")
	}
}

func TestCompletedCheckFailureIsRecordedAndNotRepeated(t *testing.T) {
	root, in := completedFixture(t)
	assembled, err := AssembleCompletedRun(context.Background(), root, in)
	if err != nil {
		t.Fatal(err)
	}
	check := CheckRequest{CompletedRequest: in.CompletedRequest, ExpectedCandidateSHA256: assembled.CandidateSHA256, Check: []string{"sh", "-c", "echo ran >> count; exit 3"}, Timeout: 5 * time.Second}
	check.RequestID = "failing-check"
	result, err := CheckCompletedDelivery(context.Background(), root, check)
	if err != nil || result.Status != "check_failed" || result.Error == "" {
		t.Fatal(result, err)
	}
	again, err := CheckCompletedDelivery(context.Background(), root, check)
	if err != nil || !again.Replayed || again.Error != result.Error {
		t.Fatal(again, err)
	}
	count, _ := os.ReadFile(filepath.Join(in.Directory, "count"))
	if string(count) != "ran\n" {
		t.Fatal("failed checker repeated", string(count))
	}
}

func TestCompletedCheckRetainsCleanupUncertaintyAndSourceFence(t *testing.T) {
	root, in := completedFixture(t)
	assembled, err := AssembleCompletedRun(context.Background(), root, in)
	if err != nil {
		t.Fatal(err)
	}
	check := CheckRequest{CompletedRequest: in.CompletedRequest, ExpectedCandidateSHA256: assembled.CandidateSHA256, Check: []string{"sh", "-c", "true"}, Timeout: 5 * time.Second}
	check.RequestID = "check"
	if err := jsonio.AtomicJSON(filepath.Join(root, "delivery-check.process.json"), model.ProcessMarker{Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCompletedDelivery(context.Background(), root, check); err == nil {
		t.Fatal("uncertain cleanup ignored")
	}
	if err := jsonio.AtomicJSON(filepath.Join(root, "delivery-check.process.json"), model.ProcessMarker{Status: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in.Directory, "source.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCompletedDelivery(context.Background(), root, check); err == nil {
		t.Fatal("changed candidate accepted")
	}
}
