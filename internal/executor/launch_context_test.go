package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestLaunchCapsuleBoundaries(t *testing.T) {
	file, c := runfixture.ContextCapsuleFixture(t)
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0600 {
		t.Fatal("capsule not private")
	}
	if code, err := launch.LaunchContextCLI([]string{"create", "--workspace", c.Workspace, "--out", file}); err == nil || code == 0 {
		t.Fatal("existing capsule overwritten")
	}
	raw, _ := os.ReadFile(file)
	cases := map[string][]byte{"case-collision": []byte(strings.Replace(string(raw), `"schema":`, `"Schema":"wrong","schema":`, 1)), "unknown": []byte(strings.Replace(string(raw), `"schema":`, `"extra":true,"schema":`, 1)), "duplicate": []byte(strings.Replace(string(raw), `"schema":`, `"session_id":"other","schema":`, 1)), "trailing": append(append([]byte{}, raw...), []byte(` {}`)...), "oversized": []byte(strings.Repeat(" ", launch.LaunchContextLimit+1)), "outside-registry": []byte(strings.Replace(string(raw), c.Registry, "/private/outside.json", 1)), "wrong-schema": []byte(strings.Replace(string(raw), launch.LaunchContextSchema, "other/v1", 1))}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(c.Workspace, name+".json")
			os.WriteFile(p, b, 0600)
			if _, err := launch.ReadLaunchCapsule(p); err == nil {
				t.Fatal("accepted malformed capsule")
			}
		})
	}
	link := filepath.Join(c.Workspace, "alias.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.ReadLaunchCapsule(link); err == nil {
		t.Fatal("accepted symlink")
	}
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", file)
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", c.Registry)
	if _, err := launch.CurrentLaunchCapsule(c.Workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.CurrentLaunchCapsule(runfixture.CanonicalContextDir(t)); err == nil {
		t.Fatal("accepted outside run")
	}
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", filepath.Join(c.Workspace, "other.json"))
	if _, err := launch.CurrentLaunchCapsule(c.Workspace); err == nil {
		t.Fatal("accepted conflicting registry")
	}
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", c.Registry)
	c.RuntimeSHA256 = strings.Repeat("0", 64)
	jsonio.AtomicJSON(file, c)
	if _, err := launch.CurrentLaunchCapsule(c.Workspace); err == nil {
		t.Fatal("accepted wrong runtime")
	}
}

// Runs in a real provider subprocess. Checks the received identity and selected
// data before acknowledging, so this is not mere environment inheritance.
func TestContextProviderHelper(t *testing.T) {
	mode := os.Getenv("IRRIGATOR_CONTEXT_TEST_HELPER")
	if mode == "" {
		return
	}
	var req model.WorkRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		os.Exit(21)
	}
	c := req.LaunchContext
	if c == nil || c.Schema != launch.WorkerContextSchema || c.RunID != req.RunID || c.TaskID != req.Task.ID || c.ParentID != req.Task.Parent || c.OperationID != req.OperationID || c.Attempt != req.Attempt || c.RequestID != req.RequestID || c.Kind != req.Kind || c.Digest != launch.WorkContextDigest(*c) {
		os.Exit(22)
	}
	if req.Context["allowed"] != "chosen" || req.Context["hidden"] != "" || len(c.ContextKeys) != 1 || c.ContextKeys[0] != "allowed" {
		os.Exit(23)
	}
	if mode == "capsule" && (c.Capsule == nil || c.Capsule.Schema != launch.LaunchContextSchema) {
		os.Exit(24)
	}
	output := map[string]string{"answer": req.Task.ID + "|" + req.Task.Parent + "|" + req.Context["allowed"]}
	if req.Task.Spawn != nil {
		output["children"] = `{"tasks":[{"id":"child","prompt":"Use selected context","requires":["answer"],"context":["allowed"]}]}`
	}
	receipt := &model.ContextReceipt{Schema: launch.ContextReceiptSchema, Digest: c.Digest, Consumer: "independent-local-fixture"}
	if mode == "mismatch" {
		receipt.Digest = strings.Repeat("0", 64)
	}
	if mode == "legacy" {
		receipt = nil
	}
	_ = json.NewEncoder(os.Stdout).Encode(model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID, Output: output, ContextReceipt: receipt})
	os.Exit(0)
}

func TestContextProviderReceiptsAndMismatch(t *testing.T) {
	for _, mode := range []string{"ok", "legacy", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("IRRIGATOR_CONTEXT_TEST_HELPER", mode)
			t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
			cell := runfixture.CanonicalContextDir(t)
			prefix := filepath.Join(cell, "1")
			req := model.WorkRequest{Protocol: model.ProtocolVersion, RunID: "run", RequestID: "request", OperationID: "operation", Attempt: 1, Kind: "task", Task: model.Task{ID: "node", Parent: "parent"}, Context: map[string]string{"allowed": "chosen"}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := External(ctx, cell, prefix, contextProvider(t), req)
			if mode == "mismatch" {
				if err == nil || model.OutcomeReason(err) != "malformed_response" {
					t.Fatalf("mismatch not fenced: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, e := os.ReadFile(prefix + ".context.json")
			if mode == "legacy" {
				if !os.IsNotExist(e) {
					t.Fatal("fabricated legacy consumption receipt")
				}
				return
			}
			if e != nil || !strings.Contains(string(b), "provider_reported_consumption") {
				t.Fatal("missing bounded receipt", e)
			}
			for _, forbidden := range []string{"chosen", "hidden", "prompt"} {
				if strings.Contains(string(b), forbidden) {
					t.Fatalf("receipt leaks %s", forbidden)
				}
			}
		})
	}
}

func TestContextEnvelopeRejectsChangedIdentityAndInputs(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	base := model.WorkRequest{Protocol: model.ProtocolVersion, RunID: "r", Task: model.Task{ID: "t", Parent: "p", Turn: &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}}, OperationID: "op", RequestID: "q", Kind: "task", Attempt: 1, Context: map[string]string{"allowed": "chosen"}}
	if err := launch.PrepareWorkContext(&base, runfixture.CanonicalContextDir(t)); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"task", "parent", "operation", "request", "attempt", "input", "turn", "digest"} {
		t.Run(field, func(t *testing.T) {
			b, _ := json.Marshal(base)
			var req model.WorkRequest
			json.Unmarshal(b, &req)
			switch field {
			case "task":
				req.Task.ID = "other"
			case "parent":
				req.Task.Parent = "other"
			case "operation":
				req.OperationID = "other"
			case "request":
				req.RequestID = "other"
			case "attempt":
				req.Attempt++
			case "input":
				req.Context["allowed"] = "changed"
			case "turn":
				req.Task.Turn.Index++
			case "digest":
				req.LaunchContext.Digest = "other"
			}
			if launch.ValidateWorkerLaunchContext(req) == nil {
				t.Fatal("accepted changed bound field")
			}
		})
	}
}

func TestContextProviderPoolRetryDoesNotReplayOrClaimNewConsumption(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	p := fakeProviderPool(t)
	req := runfixture.FakeProviderRequest("context-operation")
	req.Attempt = 1
	if err := launch.PrepareWorkContext(&req, p.Opts.Dir); err != nil {
		t.Fatal(err)
	}
	first := p.Work(context.Background(), req)
	if first.Error != "" || first.ContextReceipt == nil || first.ContextReceipt.Digest != req.LaunchContext.Digest {
		t.Fatalf("first: %+v", first)
	}
	same := p.Work(context.Background(), req)
	if same.Error != "" || same.ContextReceipt == nil || same.Output["answer"] != first.Output["answer"] {
		t.Fatalf("same request replay: %+v", same)
	}
	req.RequestID = "retry-context"
	req.Attempt = 2
	if err := launch.PrepareWorkContext(&req, p.Opts.Dir); err != nil {
		t.Fatal(err)
	}
	again := p.Work(context.Background(), req)
	if again.Error != "" || again.Output["answer"] != first.Output["answer"] || again.ContextReceipt != nil {
		t.Fatalf("new attempt replay fabricated consumption: %+v", again)
	}
}

func TestContextCapsuleAncestorReplacementCannotRedirectRead(t *testing.T) {
	_, c := runfixture.ContextCapsuleFixture(t)
	parent := filepath.Join(c.Workspace, "parent")
	parked := filepath.Join(c.Workspace, "parked")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(parent, "capsule.json")
	if err := jsonio.AtomicJSON(file, c); err != nil {
		t.Fatal(err)
	}
	outside := runfixture.CanonicalContextDir(t)
	evil := *c
	evil.SessionID = "redirected-outside"
	if err := jsonio.AtomicJSON(filepath.Join(outside, "capsule.json"), evil); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Rename(parent, parked) != nil {
				continue
			}
			if os.Symlink(outside, parent) == nil {
				_ = os.Remove(parent)
			}
			_ = os.Rename(parked, parent)
		}
	}()
	defer func() { close(stop); <-done }()
	for i := 0; i < 400; i++ {
		got, err := launch.ReadLaunchCapsule(file)
		if err == nil && got.SessionID != c.SessionID {
			t.Fatalf("ancestor race redirected capsule read: %s", got.SessionID)
		}
	}
}
