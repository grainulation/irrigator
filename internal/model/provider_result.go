package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// A complete error response is useful evidence only after the supervised process
// group has stopped. Never infer that from a dollar amount or a stored PID.
func ProviderProcessStopped(prefix string) bool {
	raw, err := ReadProcessEvidence(prefix+".process.json", 4096)
	var marker ProcessMarker
	return err == nil && json.Unmarshal(raw, &marker) == nil && marker.Status == "stopped" && marker.CleanupError == ""
}

func ClaudeOutcomeError(certainty string, err error) error {
	if certainty == OutcomeUncertain {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return TransportOutcome(WorkRequest{Kind: "task"}, err)
		}
	}
	return WithOutcome(certainty, err)
}

// Raw CLI text remains in local evidence, never public diagnostics. Authentication
// classification is advisory and does not establish billing or effect certainty.
func ClaudeResultDiagnostic(subtype, result string) (string, string) {
	lower := strings.ToLower(result)
	if strings.Contains(lower, "not logged in") || strings.Contains(lower, "please run /login") || strings.Contains(lower, "authentication failed") {
		return DiagProviderAuth, "Claude authentication required; run claude auth login, then inspect and explicitly retry the failed task"
	}
	return ProviderResultDiagnostic(subtype)
}
