package dashboard

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/textsafe"
)

func Display(s *model.State, root string) {
	displayTo(os.Stdout, s, root, terminalBound("IRRIGATOR_DISPLAY_CELLS", 20, 1, 200))
}

// displayTo keeps non-interactive controller logs bounded independently of
// graph size. Full evidence remains available through inspect and saved state.
func displayTo(out io.Writer, s *model.State, root string, limit int) {
	count := 0
	for _, row := range s.Tasks {
		if row.Status == "verified" {
			count++
		}
	}
	fmt.Fprintf(out, "%s | %s | %s | revision %d | %d/%d verified | %s\n", filepath.Base(root), s.Backend, s.Control, s.GraphRevision, count, len(s.Tasks), provider.CostStatusText(s))
	for _, p := range s.Proposals {
		if graph.TerminalDecision(s, p.ID) == nil {
			fmt.Fprintf(out, "  proposal %-22s queued; inspect with irrigator decisions\n", p.ID)
		}
	}
	tasks := s.AllTasks()
	shown := 0
	for _, spec := range tasks {
		if shown >= limit {
			break
		}
		shown++
		row := s.Tasks[spec.ID]
		fmt.Fprintf(out, "  %-22s %-10s attempts=%d", spec.ID, row.Status, row.Attempt)
		if row.Attempt > 0 && row.CostBasis == "unknown" {
			if row.Usage != nil && row.Usage.TotalTokens > 0 {
				fmt.Fprintf(out, "  cost unknown (%s)", model.FormatTokenSummary(row.Usage))
			} else {
				fmt.Fprint(out, "  cost unknown")
			}
		}
		if row.Error != "" {
			fmt.Fprintf(out, "  %s", textsafe.Terminal(row.Error))
		}
		fmt.Fprintln(out)
	}
	if len(tasks) > shown {
		fmt.Fprintf(out, "  ... %d more cells; inspect a cell with irrigator inspect %s TASK_ID\n", len(tasks)-shown, filepath.Base(root))
	}
}
