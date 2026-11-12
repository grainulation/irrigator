package provider

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestCostStatementReport(t *testing.T) {
	root := runfixture.New(t,
		runfixture.Task("c1", 0),
		runfixture.Task("c2", 0),
		runfixture.Task("c3", 0),
	)

	err := checkpoint.Update(root, func(s *model.State) error {
		// c1: reported cost
		s.Tasks["c1"].Status = "verified"
		s.Tasks["c1"].Attempt = 1
		s.Tasks["c1"].CostBasis = model.CostBasisReported
		s.Tasks["c1"].Cost = 0.25
		s.Tasks["c1"].ReportedCostMicros = 250000
		s.Tasks["c1"].Model = "claude-3-5-sonnet"

		// c2: unknown cost, with tokens
		s.Tasks["c2"].Status = "verified"
		s.Tasks["c2"].Attempt = 1
		s.Tasks["c2"].CostBasis = model.CostBasisUnknown
		s.Tasks["c2"].Usage = &model.TaskUsage{
			InputTokens:  1000,
			OutputTokens: 200,
			TotalTokens:  1200,
			Model:        "gemini-2.5-flash",
		}
		s.Tasks["c2"].Model = "gemini-2.5-flash"

		// c3: not run yet
		s.Tasks["c3"].Status = "pending"
		s.Tasks["c3"].Attempt = 0
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	s := runfixture.Read(t, root)
	rep := buildCostStatement(s, root)
	if rep.Run != filepath.Base(root) {
		t.Fatalf("expected run %s, got %s", filepath.Base(root), rep.Run)
	}
	if len(rep.Cells) != 3 {
		t.Fatalf("expected 3 cells, got %d", len(rep.Cells))
	}

	cell1 := rep.Cells[0]
	if cell1.ID != "c1" || cell1.CostBasis != model.CostBasisReported || cell1.ReportedCostMicros != 250000 || cell1.ReportedCostUSD != 0.25 {
		t.Fatalf("cell 1 mismatch: %+v", cell1)
	}

	cell2 := rep.Cells[1]
	if cell2.ID != "c2" || cell2.CostBasis != model.CostBasisUnknown || cell2.ReportedCostMicros != 0 || cell2.Usage == nil || cell2.Usage.TotalTokens != 1200 {
		t.Fatalf("cell 2 mismatch: %+v", cell2)
	}

	cell3 := rep.Cells[2]
	if cell3.ID != "c3" || cell3.CostBasis != "" || cell3.Attempts != 0 {
		t.Fatalf("cell 3 mismatch: %+v", cell3)
	}

	if rep.Totals.ReportedCostMicros != 250000 || rep.Totals.ReportedCostUSD != 0.25 {
		t.Fatalf("totals cost mismatch: %+v", rep.Totals)
	}
	if rep.Totals.Usage == nil || rep.Totals.Usage.TotalTokens != 1200 {
		t.Fatalf("totals usage mismatch: %+v", rep.Totals.Usage)
	}

	// Capture text output of costStatement
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	code, err := CostStatement(root, false)
	w.Close()
	os.Stdout = oldStdout

	if code != 0 || err != nil {
		t.Fatalf("costStatement failed: code=%d err=%v", code, err)
	}

	var buf bytes.Buffer
	io.Copy(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "Cost statement for") {
		t.Fatalf("expected header in output:\n%s", out)
	}
	if !strings.Contains(out, "c1") || !strings.Contains(out, "c2") || !strings.Contains(out, "c3") {
		t.Fatalf("missing cell IDs in output:\n%s", out)
	}
	if !strings.Contains(out, "reported") || !strings.Contains(out, "unknown") {
		t.Fatalf("missing basis in output:\n%s", out)
	}
	if !strings.Contains(out, "1200 tokens") {
		t.Fatalf("missing token count in output:\n%s", out)
	}
}
