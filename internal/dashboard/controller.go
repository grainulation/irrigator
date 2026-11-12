package dashboard

import (
	"fmt"
	"path/filepath"

	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/textsafe"
)

func Display(s *model.State, root string) {
	count := 0
	for _, row := range s.Tasks {
		if row.Status == "verified" {
			count++
		}
	}
	fmt.Printf("%s | %s | %s | revision %d | %d/%d verified | %s\n", filepath.Base(root), s.Backend, s.Control, s.GraphRevision, count, len(s.Tasks), provider.CostStatusText(s))
	for _, p := range s.Proposals {
		if graph.TerminalDecision(s, p.ID) == nil {
			fmt.Printf("  proposal %-22s queued; inspect with irrigator decisions\n", p.ID)
		}
	}
	for _, spec := range s.AllTasks() {
		row := s.Tasks[spec.ID]
		fmt.Printf("  %-22s %-10s attempts=%d", spec.ID, row.Status, row.Attempt)
		if row.Attempt > 0 && row.CostBasis == "unknown" {
			if row.Usage != nil && row.Usage.TotalTokens > 0 {
				fmt.Printf("  cost unknown (%s)", model.FormatTokenSummary(row.Usage))
			} else {
				fmt.Printf("  cost unknown")
			}
		}
		if row.Error != "" {
			fmt.Printf("  %s", textsafe.Terminal(row.Error))
		}
		fmt.Println()
	}
}
