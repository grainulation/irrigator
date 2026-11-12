package dashboard

import (
	"fmt"
	"path/filepath"

	"github.com/grainulation/irrigator/internal/session"
)

// Probe the controller's process-held lease rather than its persisted PID or
// control field. A crashed controller cannot leave this kernel lock held.
func StatusWatchController(root string) error {
	held, err := session.ProbeLock(filepath.Join(root, "controller.lock"))
	if err != nil {
		return fmt.Errorf("cannot verify controller activity: %w", err)
	}
	if !held {
		return fmt.Errorf("no active controller; status --watch stopped (workers may still be active). Inspect with irrigator debug %q; recover with irrigator resume %q", root, root)
	}
	return nil
}
