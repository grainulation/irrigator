package launch

import (
	"sort"

	"github.com/grainulation/irrigator/internal/model"
)

func ManagedHistoryEnabled(p model.ExecutionProfile) bool {
	for _, c := range p.RequiredCapabilities {
		if c == "explicit_history" {
			return true
		}
	}
	return false
}
func ExecutionProfileCapabilities(kind string, p model.ExecutionProfile) []string {
	caps := ExecutionCapabilities(kind)
	if kind == "ollama_http" && p.Limits.MaxOutputTokens > 0 {
		caps = append(caps, "hard_output_token_limit")
		sort.Strings(caps)
	}
	if ManagedHistoryEnabled(p) && kind != "claude_cli" && (kind == "mock" || kind == "codex_cli") {
		caps = append(caps, "explicit_history")
		sort.Strings(caps)
	}
	return caps
}
