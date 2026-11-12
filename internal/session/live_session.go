package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

// ActiveSessionRegistry is set by the apply/resume CLI and app-routed demos. Empty means no
// project state is written (library and test paths).
var ActiveSessionRegistry string

type sessionRegistry struct {
	Version    int       `json:"version"`
	RunID      string    `json:"run_id"`
	RunPath    string    `json:"run_path"`
	Generation int       `json:"generation"`
	StartedAt  time.Time `json:"started_at"`
}

func ResolveRegistryPath(flagValue string) (string, error) {
	if flagValue == "" {
		if inherited := os.Getenv("IRRIGATOR_SESSION_REGISTRY"); inherited != "" {
			if !filepath.IsAbs(inherited) || strings.ContainsRune(inherited, '\x00') {
				return "", errors.New("IRRIGATOR_SESSION_REGISTRY must be an absolute path without NUL characters")
			}
			return filepath.Clean(inherited), nil
		}
	}
	if flagValue == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		flagValue = filepath.Join(wd, ".irrigator", "current-session.json")
	}
	return filepath.Abs(flagValue)
}

// PanelEventActions is the event vocabulary: only these State.Events kinds are exposed, never Detail.
// The log is not claimed to be complete durable history.
var PanelEventActions = map[string]string{
	"created": "run_identity", "resumed": "run_resumed", "recovered": "recovered",
	"pause_requested": "control_requested", "down_requested": "control_requested",
	"complete": "run_complete", "blocked": "run_blocked",
	"started": "node_started", "produced": "node_produced", "verified": "node_verified",
	"failed": "node_failed", "expanded": "node_expanded", "graph_verified": "graph_verified",
	"retry_requested": "retry_requested", "cancel_requested": "cancel_requested", "cancelled": "node_cancelled",
	"notes_delivered": "node_notes",
}

type PanelEvent struct {
	ID     string    `json:"id"`
	Seq    int       `json:"seq"`
	Kind   string    `json:"kind"`
	Task   string    `json:"task,omitempty"`
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Basis  string    `json:"basis"`
}

func PanelEvents(s *model.State) []PanelEvent {
	out := []PanelEvent{}
	for i, e := range s.Events {
		action, ok := PanelEventActions[e.Kind]
		if !ok {
			continue
		}
		out = append(out, PanelEvent{ID: fmt.Sprintf("%s:%d", s.RunID, i+1), Seq: i + 1, Kind: e.Kind, Task: e.Task, Time: e.Time, Action: action, Basis: "event_log"})
	}
	if len(out) > 200 {
		out = out[len(out)-200:]
	}
	return out
}
