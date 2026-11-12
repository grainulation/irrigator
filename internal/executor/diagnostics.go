package executor

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
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
	switch auth {
	case "authenticated":
		modes = append(modes, "haiku")
		next = append(next, "Claude login is available; choose a provider and explicit spending limits before a model-backed run.")
	case "not_authenticated":
		next = append(next, "Run claude auth login, then rerun irrigator doctor.")
	case "timeout":
		next = append(next, "Claude authentication status timed out; check claude auth status before a model-backed run.")
	case "provider_error":
		next = append(next, "Claude authentication status could not be verified; run claude auth status and check your CLI version.")
	default:
		next = append(next, "Install and authenticate Claude for the haiku backend, or configure a named provider in your plan.")
	}
	agents := detectAgents()
	report := map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "go_version": runtime.Version(), "claude_path": claudePath,
		"readiness_note": "ready/local_ready describe credential-free local execution; model_ready describes the checked Claude login only. Named providers require their own preflight.",
		"ready":          true, "readiness_scope": "local_mock_execution", "local_ready": true, "model_ready": auth == "authenticated",
		"claude_installed": claudePath != "", "haiku_auth": auth, "modes": modes, "next_steps": next,
		"agents": agents}
	for _, a := range agents {
		if a.ID == "agy" {
			report["agy_version"] = a.Version
			report["agy_status"] = a.Status
			break
		}
	}
	return report
}
