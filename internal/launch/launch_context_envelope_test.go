package launch

import (
	"testing"

	"github.com/grainulation/irrigator/internal/buildinfo"
	"github.com/grainulation/irrigator/internal/model"
)

func TestLaunchContextRejectsForgedCapsuleWithValidEnvelopeDigest(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	req := model.WorkRequest{RunID: "review-run", Task: task("review-task", 0), RequestID: "request", OperationID: "operation", Attempt: 1, Kind: "task"}
	if e := PrepareWorkContext(&req, t.TempDir()); e != nil {
		t.Fatal(e)
	}
	req.LaunchContext.Capsule = &model.LaunchCapsule{Schema: "unexpected/schema", SessionID: "forged", Workspace: "/outside", Registry: "/outside/registry", RuntimeVersion: buildinfo.Version, RuntimeSHA256: "invalid", HeadlessSupported: true}
	req.LaunchContext.Digest = WorkContextDigest(*req.LaunchContext)
	if ValidateWorkerLaunchContext(req) == nil {
		t.Fatal("self-consistent envelope bypassed capsule validation")
	}
}
func TestLaunchContextTurnMetadataBoundToEnvelope(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	req := model.WorkRequest{RunID: "review-run", Task: task("review-task", 0), RequestID: "request", OperationID: "operation", Attempt: 1, Kind: "task"}
	req.Task.Turn = &model.TurnIdentity{AgentID: "agent", Role: "main", Index: 1}
	if e := PrepareWorkContext(&req, t.TempDir()); e != nil {
		t.Fatal(e)
	}
	req.Task.Turn = &model.TurnIdentity{AgentID: "other", Role: "main", Index: 1}
	if ValidateWorkerLaunchContext(req) == nil {
		t.Fatal("changed task turn identity accepted")
	}
}
