package managed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestManagedAdapterIdentityAndTools(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.01}'`)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if err != nil || response.Output["answer"] != "ok" || response.CostUSD != .01 || response.RequestID != req.RequestID {
		t.Fatalf("%+v %v", response, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "captured.json"))
	if err != nil {
		t.Fatal(err)
	}
	var captured model.WorkRequest
	if json.Unmarshal(raw, &captured) != nil || captured.RunID != req.RunID || captured.OperationID != req.OperationID || captured.RequestID != req.RequestID || captured.Task.ID != req.Task.ID || captured.Attempt != req.Attempt || launch.ValidateWorkerLaunchContext(captured) != nil {
		t.Fatal("identity/context not delivered")
	}
	var receipt ManagedDispatchReceipt
	receiptRaw, err := os.ReadFile(prefix + ".managed.json")
	if err != nil || json.Unmarshal(receiptRaw, &receipt) != nil || receipt.RunID != req.RunID || receipt.TaskID != req.Task.ID || receipt.OperationID != req.OperationID || receipt.Attempt != req.Attempt || receipt.BindingDigest != b.Digest || receipt.ReportedModel != nil || receipt.ReportedCostUSD == nil || *receipt.ReportedCostUSD != .01 {
		t.Fatalf("invalid managed receipt: %s %v", receiptRaw, err)
	}
	if _, err := os.Stat(prefix + ".context.json"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "argv.txt"))
	for _, s := range []string{"--tools\n\n", "--strict-mcp-config\n", "--disable-slash-commands\n", "--model\nrequested-model\n", "--max-budget-usd\n0.25\n"} {
		if !strings.Contains(string(args), s) {
			t.Fatalf("missing args %q", s)
		}
	}
}

func TestManagedAdapterMalformedCostAndCancellation(t *testing.T) {
	for name, script := range map[string]string{
		"cost_alias":     `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.25,"TOTAL_COST_USD":0}'`,
		"error_alias":    `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.01,"is_error":true,"IS_ERROR":false}'`,
		"null_error":     `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.01,"is_error":null}'`,
		"missing_cost":   `printf '%s' '{"type":"result","structured_output":{"answer":"ok"}}'`,
		"negative_cost":  `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":-1}'`,
		"duplicate_cost": `printf '%s' '{"type":"result","total_cost_usd":0,"total_cost_usd":0.01}'`,
		"malformed":      `printf nope`,
		"cancel":         `sleep 5`,
	} {
		t.Run(name, func(t *testing.T) {
			b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, script)
			timeout := 5 * time.Second
			if name == "cancel" {
				timeout = 300 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_, err := managedWork(ctx, dir, prefix, req, b)
			if !model.IsUncertain(err) {
				t.Fatalf("should retain hold: %v", err)
			}
			if unresolved, e := launch.UnresolvedProcess(dir, 1); e != nil || unresolved {
				t.Fatalf("cleanup %v %v", unresolved, e)
			}
		})
	}
}

func TestManagedAdapterPreDispatchRefusals(t *testing.T) {
	for _, kind := range []string{"input", "schema_input", "replacement", "history", "effort", "budget", "env"} {
		t.Run(kind, func(t *testing.T) {
			b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `exit 0`)
			switch kind {
			case "input":
				b.Profile.Limits.InputBytes = 1
			case "schema_input":
				copy := req
				if err := launch.PrepareWorkContext(&copy, dir); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(copy)
				b.Profile.Limits.InputBytes = int64(len(raw) + 1)
			case "replacement":
				if err := os.WriteFile(b.Adapter.Command[0], []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			case "history":
				b.Profile.RequiredCapabilities = append(b.Profile.RequiredCapabilities, "explicit_history")
			case "effort":
				req.Task.Effort = "high"
			case "budget":
				req.MaxCostUSD = .5
			case "env":
				b.Instance.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "env", Name: "AUTH"}}
			}
			b.Digest = ""
			b.Digest = launch.ExecutionDigest(b)
			_, err := managedWork(context.Background(), dir, prefix, req, b)
			if !model.KnownNotStarted(err) {
				t.Fatalf("should not dispatch: %v", err)
			}
			if _, err = os.Stat(filepath.Join(dir, "captured.json")); !os.IsNotExist(err) {
				t.Fatal("refused request invoked CLI")
			}
		})
	}
}

func TestManagedSupervisorNotStartedCertainty(t *testing.T) {
	for _, kind := range []string{"start_failure", "preparation_failure", "poststart_failure", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `exit 7`)
			expectedNotStarted := kind == "start_failure" || kind == "preparation_failure"
			if kind == "start_failure" {
				if err := os.WriteFile(b.Adapter.Command[0], []byte("#!/definitely/missing/interpreter\n"), 0700); err != nil {
					t.Fatal(err)
				}
				b.Adapter.ExecutableSHA256, _ = launch.ExecutableDigest(b.Adapter.Command[0])
				b.Digest = ""
				b.Digest = launch.ExecutionDigest(b)
			}
			if kind == "preparation_failure" {
				if err := os.Mkdir(prefix+".input.json", 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "timeout" {
				if err := os.WriteFile(b.Adapter.Command[0], []byte("#!/bin/sh\nsleep 5\n"), 0700); err != nil {
					t.Fatal(err)
				}
				b.Adapter.ExecutableSHA256, _ = launch.ExecutableDigest(b.Adapter.Command[0])
				b.Digest = ""
				b.Digest = launch.ExecutionDigest(b)
			}
			timeout := 5 * time.Second
			if kind == "timeout" {
				timeout = 300 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_, err := managedWork(ctx, dir, prefix, req, b)
			if model.KnownNotStarted(err) != expectedNotStarted || (!expectedNotStarted && !model.IsUncertain(err)) {
				t.Fatalf("%s certainty: %v", kind, err)
			}
		})
	}
}

func TestManagedReceiptPersistenceFailureUncertain(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.01}'`)
	if err := os.Mkdir(prefix+".managed.json", 0700); err != nil {
		t.Fatal(err)
	}
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if !model.IsUncertain(err) || response.Output != nil || response.CostUSD != .01 {
		t.Fatalf("receipt failure accepted success: %+v %v", response, err)
	}
}

func TestManagedClaudePreservesOSIdentityNotCredentialOverrides(t *testing.T) {
	t.Setenv("USER", "managed-fixture-user")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-forward-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "must-not-forward-token")
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' "${USER-unset}" > os-user
printf '%s' "${ANTHROPIC_API_KEY-unset}" > api-key-state
printf '%s' "${CLAUDE_CODE_OAUTH_TOKEN-unset}" > oauth-state
printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.01}'`)
	if _, err := managedWork(context.Background(), dir, prefix, req, b); err != nil {
		t.Fatal(err)
	}
	user, err := os.ReadFile(filepath.Join(dir, "os-user"))
	if err != nil || string(user) != "managed-fixture-user" {
		t.Fatalf("lost OS identity needed by host keychain: %q %v", user, err)
	}
	for _, file := range []string{"api-key-state", "oauth-state"} {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(raw) != "unset" {
			t.Fatal("forwarded credential override")
		}
	}
}
