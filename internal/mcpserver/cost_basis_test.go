package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestCostBasisOutcomeTransitions(t *testing.T) {
	root := runfixture.New(t,
		runfixture.Task("reported-cell", 0),
		runfixture.Task("unknown-cell", 0),
		runfixture.Task("tokens-only-cell", 0),
	)
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}

	s := runfixture.Read(t, root)
	cells := executor.CheckpointCells{Root: root}

	// 1. Admit and record reported cell
	admitted1, err := cells.BeginAttempt("reported-cell", s.Tasks["reported-cell"].OperationID, 123)
	if err != nil {
		t.Fatal(err)
	}
	err = cells.RecordOutcome(managed.CellOutcome{
		Fence:     admitted1.Fence,
		Result:    map[string]string{"answer": "ok"},
		Cost:      0.05,
		CostBasis: model.CostBasisReported,
		Model:     "claude-3-5-sonnet",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Admit and record unknown cell (no cost, no tokens)
	admitted2, err := cells.BeginAttempt("unknown-cell", s.Tasks["unknown-cell"].OperationID, 124)
	if err != nil {
		t.Fatal(err)
	}
	err = cells.RecordOutcome(managed.CellOutcome{
		Fence:       admitted2.Fence,
		Result:      map[string]string{"answer": "ok"},
		CostUnknown: true,
		CostBasis:   model.CostBasisUnknown,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Admit and record tokens-only cell (tokens, no cost)
	admitted3, err := cells.BeginAttempt("tokens-only-cell", s.Tasks["tokens-only-cell"].OperationID, 125)
	if err != nil {
		t.Fatal(err)
	}
	err = cells.RecordOutcome(managed.CellOutcome{
		Fence:       admitted3.Fence,
		Result:      map[string]string{"answer": "ok"},
		CostUnknown: true,
		CostBasis:   model.CostBasisUnknown,
		Usage: &model.TaskUsage{
			InputTokens:  500,
			OutputTokens: 50,
			TotalTokens:  550,
			Model:        "gemini-2.5-pro",
		},
		Model: "gemini-2.5-pro",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify durable state
	finalState := runfixture.Read(t, root)

	r1 := finalState.Tasks["reported-cell"]
	if r1.CostBasis != model.CostBasisReported || r1.Cost != 0.05 || r1.ReportedCostMicros != 50000 {
		t.Fatalf("reported-cell mismatch: %+v", r1)
	}

	r2 := finalState.Tasks["unknown-cell"]
	if r2.CostBasis != model.CostBasisUnknown || r2.Cost != 0 || r2.ReportedCostMicros != 0 {
		t.Fatalf("unknown-cell mismatch: %+v", r2)
	}

	r3 := finalState.Tasks["tokens-only-cell"]
	if r3.CostBasis != model.CostBasisUnknown || r3.Cost != 0 || r3.ReportedCostMicros != 0 {
		t.Fatalf("tokens-only-cell cost mismatch: %+v", r3)
	}
	if r3.Usage == nil || r3.Usage.TotalTokens != 550 || r3.Model != "gemini-2.5-pro" {
		t.Fatalf("tokens-only-cell usage mismatch: %+v", r3.Usage)
	}

	// Verify events in durable log
	var producedEvents []model.Event
	for _, ev := range finalState.Events {
		if ev.Kind == "produced" {
			producedEvents = append(producedEvents, ev)
		}
	}
	if len(producedEvents) != 3 {
		t.Fatalf("expected 3 produced events, got %d", len(producedEvents))
	}

	// Check produced detail for tokens-only cell
	var tokensProd model.Event
	for _, ev := range producedEvents {
		if ev.Task == "tokens-only-cell" {
			tokensProd = ev
			break
		}
	}
	detail, ok := tokensProd.Detail.(map[string]any)
	if !ok {
		t.Fatalf("invalid detail type: %T", tokensProd.Detail)
	}
	if detail["cost_basis"] != model.CostBasisUnknown {
		t.Fatalf("expected cost_basis unknown in event detail, got %v", detail["cost_basis"])
	}
	if detail["usage"] == nil {
		t.Fatal("expected usage in event detail")
	}

	// Verify run totals
	proj := provider.ProjectCost(finalState)
	if proj.KnownReportedUSD != 0.05 {
		t.Fatalf("expected known reported USD 0.05, got %f", proj.KnownReportedUSD)
	}
	if proj.Tokens == nil || proj.Tokens.TotalTokens != 550 {
		t.Fatalf("expected 550 total tokens in projection, got %+v", proj.Tokens)
	}

	statusText := provider.CostStatusText(finalState)
	if !strings.Contains(statusText, "reported estimate USD 0.0500") {
		t.Fatalf("expected reported estimate in status text, got: %s", statusText)
	}
	if !strings.Contains(statusText, "550 tokens") {
		t.Fatalf("expected token summary in status text, got: %s", statusText)
	}
}

func TestCostBasisMCPExposure(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("c1", 0))

	err := checkpoint.Update(root, func(s *model.State) error {
		row := s.Tasks["c1"]
		row.Status = "verified"
		row.Attempt = 1
		row.CostBasis = model.CostBasisUnknown
		row.Usage = &model.TaskUsage{
			InputTokens:  100,
			OutputTokens: 20,
			TotalTokens:  120,
			Model:        "claude-3-7-sonnet",
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	tools := &mcpTools{}
	_, out, err := tools.runStatus(context.Background(), nil, mcpRunInput{Run: root})
	if err != nil {
		t.Fatal(err)
	}

	cells, ok := out["cells"].([]any)
	if !ok || len(cells) != 1 {
		t.Fatalf("expected 1 cell, got %v", out["cells"])
	}
	cell := cells[0].(map[string]any)
	if cell["cost_basis"] != model.CostBasisUnknown {
		t.Fatalf("expected cost_basis unknown, got %v", cell["cost_basis"])
	}
	if cell["usage"] == nil {
		t.Fatal("expected usage in MCP cell output")
	}
}
