package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func executionFixture(t *testing.T, kind string) (string, model.ExecutionCatalog, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter := model.ExecutionAdapter{Kind: "mock", Protocol: model.ProtocolVersion}
	instance := model.ExecutionInstance{Adapter: "adapter", CredentialRefs: []model.ExecutionCredentialRef{}}
	profile := model.ExecutionProfile{Instance: "account", Model: "mock", Effort: "none", RequiredCapabilities: []string{"structured_strings", "outcome_certainty"}, Limits: model.ExecutionLimits{InputBytes: 1024, OutputBytes: 1024}, Tools: []string{}, OperationBudgetUSD: "0"}
	marker := filepath.Join(root, "must-not-execute")
	if kind == "claude_cli" {
		exe := filepath.Join(root, "claude-fixture")
		data := []byte(fmt.Sprintf("#!/bin/sh\ntouch %q\n", marker))
		if err := os.WriteFile(exe, data, 0700); err != nil {
			t.Fatal(err)
		}
		hash, err := ExecutableDigest(exe)
		if err != nil {
			t.Fatal(err)
		}
		adapter = model.ExecutionAdapter{Kind: kind, Protocol: model.ProtocolVersion, Command: []string{exe}, ExecutableSHA256: hash}
		instance.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "env", Name: "PROFILE_TEST_SECRET"}}
		profile.Model = "requested-model"
		profile.Effort = "low"
		profile.OperationBudgetUSD = "0.25"
	}
	c := model.ExecutionCatalog{Schema: ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{"adapter": adapter}, Instances: map[string]model.ExecutionInstance{"account": instance}, Profiles: map[string]model.ExecutionProfile{"small": profile}, DefaultProfile: "small"}
	path := filepath.Join(root, "catalog.json")
	writeExecutionFixture(t, path, c)
	return path, c, marker
}

func writeExecutionFixture(t *testing.T, path string, c model.ExecutionCatalog) {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func task(id string, delay float64, deps ...string) model.Task {
	return model.Task{ID: id, Prompt: "produce answer", Requires: []string{"answer"}, DependsOn: deps, MockDelay: delay}
}
