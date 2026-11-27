package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestDoctorSelectedCodexDoesNotRequireClaude(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--version) printf 'codex 0.163.0\\n';;\nlogin) printf 'Logged in using a private account\\n' >&2;;\n*) printf 'help\\n';;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	prefs := filepath.Join(canonicalDir, "preferences.json")
	if err := os.WriteFile(prefs, []byte(`{"schema":"irrigator.preferences/v1","launcher":{"provider":"codex","model":"gpt-6.2-sol","effort":"high"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv(launch.ExecutionPreferencesEnv, prefs)
	report := DoctorReport()
	if report["claude_installed"] != false || report["model_ready"] != true {
		t.Fatalf("Claude absence broke selected Codex readiness: %v", report)
	}
	health := report["execution_health"].(map[string]any)
	if health["authentication"] != "authenticated" || health["model_support"] != "unknown" || health["generation"] != "not_probed" {
		t.Fatalf("overclaimed readiness: %v", health)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "private account") || strings.Contains(string(raw), "Install and authenticate Claude") {
		t.Fatalf("private account output or forced Claude onboarding: %s", raw)
	}
}

func TestDoctorLocalInventoryUsesExplicitEndpointWithoutGeneration(t *testing.T) {
	var requested atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Add(1)
		if r.URL.Path != "/api/tags" || r.Method != http.MethodGet {
			t.Errorf("healthcheck dispatched instead of listing: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"local-model:latest"}]}`))
	}))
	defer server.Close()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefs := filepath.Join(dir, "preferences.json")
	body, _ := json.Marshal(map[string]any{"schema": launch.ExecutionPreferencesSchema, "launcher": launch.ExecutionPreference{Provider: "ollama", Model: "local-model:latest", Effort: "none", Endpoint: server.URL}})
	if err := os.WriteFile(prefs, body, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv(launch.ExecutionPreferencesEnv, prefs)
	report := DoctorReport()
	health, ok := report["execution_health"].(map[string]any)
	if !ok || requested.Load() != 1 || health["status"] != "inventory_available" || health["generation"] != "not_probed" || report["model_ready"] != false {
		t.Fatalf("overclaimed local inventory: %v, requests=%d", report, requested.Load())
	}
}

func TestManagedCodexDispatchCarriesHighEffort(t *testing.T) {
	b, req, dir, prefix := codexAdapterFixture(t, runfixture.CodexFixtureSuccess)
	b.Profile.Effort = "high"
	b.Digest = ""
	b.Digest = launch.ExecutionDigest(b)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if err != nil || response.Output["answer"] != "ok" {
		t.Fatalf("high-effort fixture failed: %+v %v", response, err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil || !strings.Contains(string(args), "model_reasoning_effort=\"high\"") || strings.Contains(string(args), "model_reasoning_effort=\"low\"") {
		t.Fatalf("child invocation lost effort: %s %v", args, err)
	}
}
