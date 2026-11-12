package provider

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

type costCellReport struct {
	ID                 string           `json:"id"`
	Status             string           `json:"status"`
	Attempts           int              `json:"attempts"`
	CostBasis          string           `json:"cost_basis"`
	ReportedCostMicros int64            `json:"reported_cost_micros"`
	ReportedCostUSD    float64          `json:"reported_cost_usd"`
	Usage              *model.TaskUsage `json:"usage,omitempty"`
	Model              string           `json:"model,omitempty"`
}

type costTotalsReport struct {
	ReportedCostMicros    int64            `json:"reported_cost_micros"`
	ReportedCostUSD       float64          `json:"reported_cost_usd"`
	UnknownCostOperations int              `json:"unknown_cost_operations"`
	Usage                 *model.TaskUsage `json:"usage,omitempty"`
}

type costStatementReport struct {
	Run    string           `json:"run"`
	Cells  []costCellReport `json:"cells"`
	Totals costTotalsReport `json:"totals"`
}

func buildCostStatement(s *model.State, root string) costStatementReport {
	runName := filepath.Base(root)
	summary := ProjectCost(s)

	cells := make([]costCellReport, 0, len(s.Tasks))
	var totalMicros int64
	for _, spec := range s.AllTasks() {
		row := s.Tasks[spec.ID]
		basis := row.CostBasis
		if basis == "" && row.Attempt > 0 {
			if row.CostUnknown {
				basis = model.CostBasisUnknown
			} else {
				basis = model.CostBasisReported
			}
		}
		totalMicros += row.CostMicros()
		c := costCellReport{
			ID:                 spec.ID,
			Status:             row.Status,
			Attempts:           row.Attempt,
			CostBasis:          basis,
			ReportedCostMicros: row.CostMicros(),
			ReportedCostUSD:    row.Cost,
			Usage:              row.Usage,
			Model:              row.Model,
		}
		cells = append(cells, c)
	}
	for _, row := range s.ArchivedTasks {
		totalMicros += row.CostMicros()
	}

	totals := costTotalsReport{
		ReportedCostMicros:    totalMicros,
		ReportedCostUSD:       cost.MicrosToFloat(totalMicros),
		UnknownCostOperations: summary.UnknownOperations,
		Usage:                 summary.Tokens,
	}

	return costStatementReport{
		Run:    runName,
		Cells:  cells,
		Totals: totals,
	}
}

func CostStatement(root string, asJSON bool) (int, error) {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return 1, err
	}
	rep := buildCostStatement(s, root)
	if asJSON {
		return 0, jsonio.PrintJSON(rep)
	}

	fmt.Printf("Cost statement for %s\n", rep.Run)
	fmt.Printf("%-22s %-10s %-8s %-10s %-14s %-20s %s\n", "CELL", "STATUS", "ATTEMPTS", "BASIS", "REPORTED USD", "MODEL", "TOKENS")
	fmt.Println(strings.Repeat("-", 90))
	for _, cell := range rep.Cells {
		costStr := fmt.Sprintf("$%.4f", cell.ReportedCostUSD)
		if cell.CostBasis == model.CostBasisUnknown {
			costStr = "unknown"
		} else if cell.Attempts == 0 {
			costStr = "-"
		}
		basisStr := cell.CostBasis
		if basisStr == "" {
			basisStr = "-"
		}
		modelStr := cell.Model
		if modelStr == "" {
			modelStr = "-"
		}
		tokenStr := "-"
		if cell.Usage != nil && cell.Usage.TotalTokens > 0 {
			tokenStr = model.FormatTokenSummary(cell.Usage)
		}
		fmt.Printf("%-22s %-10s %-8d %-10s %-14s %-20s %s\n", cell.ID, cell.Status, cell.Attempts, basisStr, costStr, modelStr, tokenStr)
	}
	fmt.Println(strings.Repeat("-", 90))
	fmt.Printf("Totals: reported estimate USD %.4f (%d µ$)", rep.Totals.ReportedCostUSD, rep.Totals.ReportedCostMicros)
	if rep.Totals.UnknownCostOperations > 0 {
		fmt.Printf(" | %d unknown-cost operation(s)", rep.Totals.UnknownCostOperations)
	}
	if rep.Totals.Usage != nil && rep.Totals.Usage.TotalTokens > 0 {
		fmt.Printf(" | %s", model.FormatTokenSummary(rep.Totals.Usage))
	}
	fmt.Println()
	return 0, nil
}
