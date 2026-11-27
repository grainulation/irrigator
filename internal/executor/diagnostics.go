package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/providerenv"
)

// Query only login status: no prompts, provider calls, or raw account details in the report.
func claudeAuthStatus(path string, deadline time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "status", "--json")
	cmd.Env = providerenv.Claude()
	cmd.WaitDelay = 100 * time.Millisecond
	var out launch.AuthStatusBuffer
	cmd.Stdout = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return "timeout"
	}
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if !out.Overflow && json.Unmarshal(out.Bytes(), &status) == nil && status.LoggedIn != nil {
		if !*status.LoggedIn {
			return "not_authenticated"
		}
		if err == nil {
			return "authenticated"
		}
	}
	return "provider_error"
}
func DoctorReport() map[string]any {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		claudePath = ""
	}
	auth := "not_installed"
	modes := []string{"mock"}
	next := []string{"Run irrigator demo --run ./runs/first-demo to try local execution without credentials."}
	if claudePath != "" {
		auth = claudeAuthStatus(claudePath, 3*time.Second)
	}
	if auth == "authenticated" {
		modes = append(modes, "haiku")
	}
	// Discovery is neutral: a missing Claude install is not an instruction to
	// replace the selected host/provider or proof that another route is broken.
	next = append(next, "Run irrigator agents to inspect installed routes; use an explicit profile or launcher preferences. Discovery never enables multi-model routing.")
	agents := detectAgents()
	report := map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "go_version": runtime.Version(), "claude_path": claudePath,
		"readiness_note": "ready/local_ready describe local mock execution. model_ready is selected-route passive preflight readiness, not model entitlement, quota or a generation check. Authentication and inventory checks remain separate.",
		"ready":          true, "readiness_scope": "local_mock_execution", "local_ready": true, "model_ready": false,
		"claude_installed": claudePath != "", "haiku_auth": auth, "modes": modes, "next_steps": next,
		"agents": agents}
	selection, selectionErr := launch.ResolveExecutionPreference(launch.ExecutionPreference{}, "", true)
	if selectionErr != nil {
		report["selection_error"] = selectionErr.Error()
		report["selection"] = nil
	} else {
		report["selection"] = selection
		health := selectedExecutionHealth(selection, agents, auth)
		report["execution_health"] = health
		report["model_ready"] = health["status"] == "ready_for_preflight"
		if health["authentication"] == "authenticated" && selection.Preference.Provider != "claude" {
			report["modes"] = append(modes, selection.Preference.Provider)
		}
	}
	for _, a := range agents {
		if a.ID == "agy" {
			report["agy_version"] = a.Version
			report["agy_status"] = a.Status
			break
		}
	}
	return report
}

// codexAuthStatus inspects only login status, discarding all raw account output.
// It never invokes exec, submits a prompt or performs login.
func codexAuthStatus(path string, deadline time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "login", "status")
	cmd.WaitDelay = 100 * time.Millisecond
	var stdout, stderr launch.AuthStatusBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "timeout"
	}
	if stdout.Overflow || stderr.Overflow {
		return "provider_error"
	}
	text := strings.ToLower(strings.TrimSpace(string(stdout.Bytes()) + "\n" + string(stderr.Bytes())))
	if strings.HasPrefix(text, "not logged in") || strings.HasPrefix(text, "not authenticated") {
		return "not_authenticated"
	}
	if err == nil && strings.HasPrefix(text, "logged in") {
		return "authenticated"
	}
	return "provider_error"
}

func selectedExecutionHealth(selection launch.ExecutionSelection, agents []agentStatus, claudeAuth string) map[string]any {
	p := selection.Preference
	if p.Provider == "ollama" && p.Endpoint != "" {
		transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, ResponseHeaderTimeout: time.Second, MaxResponseHeaderBytes: 16384}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		// Explicit endpoints are validated by preference resolution and cannot
		// route through proxies, redirects or non-loopback addresses.
		agents = []agentStatus{detectOllamaAgent(client, p.Endpoint+"/api/tags", false)}
	}
	health := map[string]any{"provider": p.Provider, "requested_model": p.Model, "effort": p.Effort, "source": selection.Source,
		"checked_at": time.Now().UTC().Format(time.RFC3339), "status": "not_installed", "authentication": "unchecked", "model_support": "unknown", "generation": "not_probed", "policy": "selected", "endpoint": p.Endpoint}
	for _, a := range agents {
		if a.ID != p.Provider {
			continue
		}
		if !a.Available {
			health["status"] = "unavailable"
			return health
		}
		health["status"] = "authentication_unchecked"
		switch p.Provider {
		case "claude":
			health["authentication"] = claudeAuth
		case "codex":
			if path, err := exec.LookPath("codex"); err == nil {
				health["authentication"] = codexAuthStatus(path, 3*time.Second)
			}
		case "ollama":
			health["authentication"] = "not_required"
			health["status"] = "model_not_listed"
			for _, name := range a.Models {
				if name == p.Model {
					health["model_support"] = "listed_not_attested"
					health["status"] = "inventory_available"
					break
				}
			}
			return health
		}
		if health["authentication"] == "authenticated" {
			health["status"] = "ready_for_preflight"
		}
		if health["authentication"] == "not_authenticated" {
			health["status"] = "not_authenticated"
		}
		if health["authentication"] == "timeout" || health["authentication"] == "provider_error" {
			health["status"] = health["authentication"]
		}
		return health
	}
	return health
}
