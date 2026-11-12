package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func wireContextRequest() model.WorkRequest {
	return model.WorkRequest{Protocol: model.ProtocolVersion, RunID: "wire-run", RequestID: "wire-request", OperationID: "wire-operation", Attempt: 1, Kind: "task", Task: model.Task{ID: "root"}}
}
func contextWireRoundTrip(t *testing.T, req model.WorkRequest) model.WorkRequest {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var received model.WorkRequest
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatal(err)
	}
	return received
}

// Root tasks construct Dependencies={} before the process/HTTP boundary, but
// WorkRequest omitempty makes the receiver see nil after the JSON round trip.
func TestLaunchContextWireRoundTrip(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	contexts := map[string]map[string]string{"nil": nil, "empty": {}, "selected": {"source": "pinned input"}}
	dependencies := map[string]map[string]map[string]string{"nil": nil, "empty": {}, "output": {"parent": {"answer": "42"}}, "nil-child": {"parent": nil}, "empty-child": {"parent": {}}}
	for cn, context := range contexts {
		for dn, deps := range dependencies {
			t.Run(cn+"/"+dn, func(t *testing.T) {
				req := wireContextRequest()
				req.Context = context
				req.Dependencies = deps
				if err := launch.PrepareWorkContext(&req, t.TempDir()); err != nil {
					t.Fatal(err)
				}
				if err := launch.ValidateWorkerLaunchContext(req); err != nil {
					t.Fatalf("controller context: %v", err)
				}
				received := contextWireRoundTrip(t, req)
				if err := launch.ValidateWorkerLaunchContext(received); err != nil {
					t.Fatalf("serialized worker context: %v", err)
				}
				if received.LaunchContext.Digest != req.LaunchContext.Digest {
					t.Fatal("wire changed envelope digest")
				}
				req.LaunchContext.InputSHA256 = legacyWireInputDigest(context, deps)
				req.LaunchContext.Digest = launch.WorkContextDigest(*req.LaunchContext)
				if err := launch.ValidateWorkerLaunchContext(contextWireRoundTrip(t, req)); err != nil {
					t.Fatalf("legacy serialized worker context: %v", err)
				}
			})
		}
	}
}

// Reproduce the old serialization independently to test read compatibility.
func legacyWireInputDigest(context map[string]string, deps map[string]map[string]string) string {
	raw, _ := json.Marshal(struct {
		Context      map[string]string
		Dependencies map[string]map[string]string
	}{context, deps})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func TestLaunchContextWireCanonicalEmptyAndLegacy(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	var canonical string
	for _, context := range []map[string]string{nil, {}} {
		for _, deps := range []map[string]map[string]string{nil, {}} {
			req := wireContextRequest()
			req.Context = context
			req.Dependencies = deps
			if err := launch.PrepareWorkContext(&req, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if canonical == "" {
				canonical = req.LaunchContext.InputSHA256
			}
			if req.LaunchContext.InputSHA256 != canonical {
				t.Error("empty and nil maps have different canonical input digests")
			}
			req.LaunchContext.InputSHA256 = legacyWireInputDigest(context, deps)
			req.LaunchContext.Digest = launch.WorkContextDigest(*req.LaunchContext)
			original := *req.LaunchContext
			received := contextWireRoundTrip(t, req)
			if err := launch.ValidateWorkerLaunchContext(received); err != nil {
				t.Errorf("legacy empty-map envelope: %v", err)
			}
			if received.LaunchContext.InputSHA256 != original.InputSHA256 || received.LaunchContext.Digest != original.Digest {
				t.Error("validator rewrote stored context")
			}
		}
	}
}
func TestLaunchContextWireRejectsTamperedInputs(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	for _, legacy := range []bool{false, true} {
		for name, mutate := range map[string]func(*model.WorkRequest){
			"changed-context":  func(r *model.WorkRequest) { r.Context["source"] = "changed" },
			"removed-context":  func(r *model.WorkRequest) { r.Context = nil },
			"added-context":    func(r *model.WorkRequest) { r.Context["extra"] = "changed" },
			"added-dependency": func(r *model.WorkRequest) { r.Dependencies = map[string]map[string]string{"other": {"answer": "42"}} },
			"input-digest":     func(r *model.WorkRequest) { r.LaunchContext.InputSHA256 = legacyWireInputDigest(nil, nil) },
		} {
			t.Run(name+map[bool]string{false: "/canonical", true: "/legacy"}[legacy], func(t *testing.T) {
				req := wireContextRequest()
				req.Context = map[string]string{"source": "pinned input"}
				req.Dependencies = map[string]map[string]string{}
				if err := launch.PrepareWorkContext(&req, t.TempDir()); err != nil {
					t.Fatal(err)
				}
				if legacy {
					req.LaunchContext.InputSHA256 = legacyWireInputDigest(req.Context, req.Dependencies)
					req.LaunchContext.Digest = launch.WorkContextDigest(*req.LaunchContext)
				}
				received := contextWireRoundTrip(t, req)
				mutate(&received)
				if launch.ValidateWorkerLaunchContext(received) == nil {
					t.Fatal("tampered input accepted")
				}
			})
		}
	}
	for _, child := range []map[string]string{nil, {}} {
		req := wireContextRequest()
		req.Dependencies = map[string]map[string]string{"parent": child}
		if err := launch.PrepareWorkContext(&req, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		received := contextWireRoundTrip(t, req)
		if child == nil {
			received.Dependencies["parent"] = map[string]string{}
		} else {
			received.Dependencies["parent"] = nil
		}
		if launch.ValidateWorkerLaunchContext(received) == nil {
			t.Fatal("nested dependency null/object alteration accepted")
		}
	}
}

// This worker validates the same full envelope as the provider adapter before
// acknowledging it, not just the envelope identity.
func TestLaunchContextWireProcessHelper(t *testing.T) {
	if os.Getenv("IRRIGATOR_WIRE_CONTEXT_TEST") != "1" {
		return
	}
	var req model.WorkRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		os.Exit(31)
	}
	if req.LaunchContext == nil || launch.ValidateWorkerLaunchContext(req) != nil {
		os.Exit(32)
	}
	response := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID,
		Output:         map[string]string{"answer": "validated-wire-input"},
		ContextReceipt: &model.ContextReceipt{Schema: launch.ContextReceiptSchema, Digest: req.LaunchContext.Digest, Consumer: "wire-validation-fixture"}}
	if json.NewEncoder(os.Stdout).Encode(response) != nil {
		os.Exit(33)
	}
	os.Exit(0)
}
func TestLaunchContextWireExternalProcess(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	t.Setenv("IRRIGATOR_WIRE_CONTEXT_TEST", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cell := runfixture.CanonicalContextDir(t)
	req := wireContextRequest()
	req.Context = map[string]string{"source": "pinned input"}
	req.Dependencies = map[string]map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := External(ctx, cell, filepath.Join(cell, "1"), model.CommandSpec{Command: []string{exe, "-test.run=^TestLaunchContextWireProcessHelper$"}, Env: []string{"IRRIGATOR_WIRE_CONTEXT_TEST"}, Replay: "safe"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if response.Output["answer"] != "validated-wire-input" || response.ContextReceipt == nil {
		t.Fatal("worker did not validate and acknowledge the serialized context")
	}
}

func TestLaunchContextWireDependencyTampering(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	for name, mutate := range map[string]func(*model.WorkRequest){
		"changed-value":  func(r *model.WorkRequest) { r.Dependencies["parent"]["answer"] = "43" },
		"removed-field":  func(r *model.WorkRequest) { delete(r.Dependencies["parent"], "answer") },
		"removed-parent": func(r *model.WorkRequest) { delete(r.Dependencies, "parent") },
		"renamed-parent": func(r *model.WorkRequest) {
			r.Dependencies["other"] = r.Dependencies["parent"]
			delete(r.Dependencies, "parent")
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := wireContextRequest()
			req.Dependencies = map[string]map[string]string{"parent": {"answer": "42"}}
			if err := launch.PrepareWorkContext(&req, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			received := contextWireRoundTrip(t, req)
			mutate(&received)
			if launch.ValidateWorkerLaunchContext(received) == nil {
				t.Fatal("tampered dependency artifact accepted")
			}
		})
	}
}
