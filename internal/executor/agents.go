package executor

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
)

// agyMinVersion is the minimum required version of the agy CLI (agy acp, --gemini-dir, ACP shutdown fix).
const agyMinVersion = "1.5.3"

// agentStatus is one agent or model provider available to this machine. The
// main agent is the one you talk to; swarm workers are what tasks run on, and
// one swarm may mix any available workers through execution profiles.
type agentStatus struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Available      bool     `json:"available"`
	Status         string   `json:"status,omitempty"`
	Version        string   `json:"version,omitempty"`
	Detail         string   `json:"detail"`
	MainAgent      bool     `json:"main_agent"`
	SwarmWorker    bool     `json:"swarm_worker"`
	Profile        string   `json:"profile,omitempty"`
	Models         []string `json:"models,omitempty"`
	Authentication string   `json:"authentication,omitempty"`
}

func parseAgentVersion(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	fields := strings.Split(v, ".")
	if len(fields) < 2 {
		return [3]int{}, false
	}
	var parts [3]int
	for i := 0; i < len(fields) && i < 3; i++ {
		digits := ""
		for _, r := range fields[i] {
			if r >= '0' && r <= '9' {
				digits += string(r)
			} else {
				break
			}
		}
		if digits == "" {
			return [3]int{}, false
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return [3]int{}, false
		}
		parts[i] = n
	}
	return parts, true
}

// cliVersion runs `NAME --version` with a short timeout; it never logs in or
// reads credentials.
func cliVersion(name string) (string, string, bool) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	version := ""
	if err == nil {
		for _, field := range strings.Fields(string(out)) {
			if strings.IndexFunc(field, func(r rune) bool { return r >= '0' && r <= '9' }) == 0 {
				version = strings.Trim(field, "(),")
				break
			}
		}
	}
	return path, version, true
}

const agentModelCatalogLimit = 1 << 20

// readAgentModels bounds local discovery and refuses incomplete or malformed
// catalogs. Model names are data, not commands or execution authorization.
func readAgentModels(body io.Reader) ([]string, error) {
	data, err := io.ReadAll(io.LimitReader(body, agentModelCatalogLimit+1))
	if err != nil || len(data) > agentModelCatalogLimit {
		return nil, fmt.Errorf("model catalog exceeds discovery limit or could not be read")
	}
	var tags struct {
		Models *[]struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &tags); err != nil || tags.Models == nil || len(*tags.Models) > 1024 {
		return nil, fmt.Errorf("invalid model catalog")
	}
	seen := make(map[string]bool)
	for _, model := range *tags.Models {
		if !launch.ExecutionModel.MatchString(model.Name) {
			return nil, fmt.Errorf("invalid model name")
		}
		seen[model.Name] = true
	}
	models := make([]string, 0, len(seen))
	for name := range seen {
		models = append(models, name)
	}
	sort.Strings(models)
	return models, nil
}

func detectOllamaAgent(client *http.Client, endpoint string, installed bool) agentStatus {
	ollama := agentStatus{ID: "ollama", Name: "Ollama", SwarmWorker: true, Profile: "ollama:MODEL"}
	resp, err := client.Get(endpoint)
	if err != nil {
		if installed {
			ollama.Detail = "Not running. Start Ollama to use local models as swarm workers."
		} else {
			ollama.Detail = "Optional. Install Ollama to run swarm workers on local models."
		}
		return ollama
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ollama.Detail = fmt.Sprintf("Ollama responded, but its model list is unavailable (HTTP %d).", resp.StatusCode)
		return ollama
	}
	models, err := readAgentModels(resp.Body)
	if err != nil {
		ollama.Detail = "Ollama responded, but its model list could not be read."
		return ollama
	}
	if len(models) == 0 {
		ollama.Detail = "Running locally, but no models are installed. Add a model to use local swarm workers."
		return ollama
	}
	ollama.Available, ollama.Models = true, models
	ollama.Detail = fmt.Sprintf("Running locally with %d installed model(s). Local swarm workers.", len(models))
	return ollama
}

func detectAgents() []agentStatus {
	agents := []agentStatus{}
	for _, host := range []struct {
		id, name string
		worker   bool
	}{
		{"claude", "Claude Code", true},
		{"codex", "Codex", true},
		{"gemini", "Gemini CLI", false},
		{"agy", "Antigravity", false},
	} {
		a := agentStatus{ID: host.id, Name: host.name, MainAgent: true, SwarmWorker: host.worker, Authentication: "unchecked"}
		if host.worker {
			a.Profile = host.id + ":MODEL"
		}
		if _, version, ok := cliVersion(host.id); ok {
			if host.id == "agy" {
				parts, pok := parseAgentVersion(version)
				minParts, _ := parseAgentVersion(agyMinVersion)
				if !pok {
					a.Available = false
					a.Status = "unknown_version"
					a.Version = version
					a.Detail = fmt.Sprintf("agy version unreadable (%s); need >= %s (agy acp, --gemini-dir, ACP shutdown fix)", version, agyMinVersion)
				} else if !versionAtLeast(parts, minParts) {
					a.Available = false
					a.Status = "too_old"
					a.Version = version
					a.Detail = fmt.Sprintf("too old: found %s, need >= %s (agy acp, --gemini-dir, ACP shutdown fix)", version, agyMinVersion)
				} else {
					a.Available = true
					a.Status = "ok"
					a.Version = version
					a.Detail = "Installed; authentication has not been checked."
				}
			} else {
				a.Available, a.Status, a.Version, a.Detail = true, "ok", version, "Installed; authentication has not been checked."
			}
		} else {
			a.Status = "not_installed"
			a.Detail = "Install the " + host.name + " CLI to use it."
		}
		agents = append(agents, a)
	}
	_, ollamaCLIError := exec.LookPath("ollama")
	client := http.Client{Timeout: 600 * time.Millisecond, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	agents = append(agents, detectOllamaAgent(&client, "http://127.0.0.1:11434/api/tags", ollamaCLIError == nil))
	agents = append(agents, agentStatus{ID: "mock", Name: "Mock workers", Available: true, Status: "ok", SwarmWorker: true,
		Detail: "Free dry runs with no model calls.", Profile: "--backend mock"})
	return agents
}

func AgentsCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print structured output")
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		return 2, err
	}
	agents := detectAgents()
	if *asJSON {
		return 0, jsonio.PrintJSON(map[string]any{"agents": agents})
	}
	fmt.Println("Agents on this machine. The main agent is the one you talk to; swarm tasks can use any available worker, so one swarm can mix models.")
	fmt.Println("Installed cloud CLIs still require authentication; discovery does not verify sign-in.")
	for _, a := range agents {
		mark := "–"
		if a.Available {
			mark = "✓"
		}
		roles := []string{}
		if a.MainAgent {
			roles = append(roles, "main agent")
		}
		if a.SwarmWorker {
			roles = append(roles, "swarm worker")
		}
		fmt.Printf("\n%s %-13s %-10s %s\n  %s\n", mark, a.Name, a.Version, strings.Join(roles, " · "), a.Detail)
	}
	return 0, nil
}
