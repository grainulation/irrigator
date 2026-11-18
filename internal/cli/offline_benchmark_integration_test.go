//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflineBenchmarkRealMockRunsAndReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "benchmark")
	if code, err := offlineBenchmarkCLI([]string{"--dir", dir, "--runs", "1"}); err != nil || code != 0 {
		t.Fatalf("benchmark: %d %v", code, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "benchmark.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report offlineBenchmark
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Cells) != 2 || report.PaidCalls != 0 || report.ModelQualityMeasured || report.SavingsEstablished {
		t.Fatalf("false benchmark scope: %+v", report)
	}
	for _, cell := range report.Cells {
		if !cell.Passed || cell.PlanSHA256 == "" || cell.Cost.KnownReportedUSD != 0 {
			t.Fatalf("invalid benchmark cell: %+v", cell)
		}
	}
	if report.Cells[0].PlanSHA256 != report.Cells[1].PlanSHA256 {
		t.Fatal("arms used different plans")
	}
}
