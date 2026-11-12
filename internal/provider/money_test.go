package provider

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestMoneyParse(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
	}{
		{"0", 0}, {"-0", 0}, {"0.1", 100000}, {"3", 3000000}, {"3.000001", 3000001},
		{"0.000001", 1}, {"1e-6", 1}, {"1.5e2", 150000000}, {"12345.678901", 12345678901},
		// More than six decimals: half to even, once.
		{"0.0000005", 0}, {"0.0000015", 2}, {"0.0000025", 2}, {"0.0000035", 4},
		{"0.00000051", 1}, {"0.00000049", 0}, {"0.0000051", 5}, {"0.0000049", 5}, {"0.0000004", 0}, {"1.2345675", 1234568}, {"1.2345665", 1234566},
		{"0.00000000000000000000001", 0},
	} {
		got, err := cost.ParseMicros(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseMicros(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
}

func TestMoneyParseRejects(t *testing.T) {
	for _, in := range []string{"", "-1", "-0.0000001", "NaN", "Inf", "+Inf", "abc", "1.2.3", ".5", "+1", "1e", "1e999", "0x10",
		"1000000001", "1e10", "99999999999999999999", "1" + strings.Repeat("0", 400)} {
		if got, err := cost.ParseMicros(in); err == nil {
			t.Errorf("parseMicros(%q) = %d, want error", in, got)
		}
	}
	for _, f := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1), 1e300} {
		if _, ok := cost.MicrosFromFloat(f); ok {
			t.Errorf("microsFromFloat(%v) accepted", f)
		}
	}
	if m, ok := cost.Limit(1e300); !ok || m != math.MaxInt64 {
		t.Errorf("huge finite ceiling must be effectively unlimited: %d %v", m, ok)
	}
	if _, ok := cost.Limit(math.NaN()); ok {
		t.Error("NaN ceiling accepted")
	}
}

func TestMoneyJSONNumberAndString(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"a":0.1,"b":"0.2","c":3,"d":1.0000005}`))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int64{"a": 100000, "b": 200000, "c": 3000000, "d": 1000000} {
		if got, ok := runfixture.MicrosFromNumber(m[key]); !ok || got != want {
			t.Errorf("%s = %d, %v; want %d", key, got, ok, want)
		}
	}
	if _, ok := runfixture.MicrosFromNumber(true); ok {
		t.Error("bool accepted")
	}
	// The wire reader decodes the raw number: 17 significant digits survive.
	if got, ok := managed.FiniteReportedCost(json.Number("3.0000005")); !ok || got != 3 {
		t.Errorf("finiteReportedCost = %v %v", got, ok)
	}
	if got, ok := managed.FiniteReportedCost(json.Number("3.0000005000000005")); !ok || got != 3.000001 {
		t.Errorf("above half rounds up: %v %v", got, ok)
	}
	if _, ok := managed.FiniteReportedCost(0.1); ok {
		t.Error("float64 must not be accepted as a raw number")
	}
	if _, ok := managed.FiniteReportedCost(json.Number("-0.1")); ok {
		t.Error("negative accepted")
	}
}

func TestMoneyFormat(t *testing.T) {
	for micros, want := range map[int64]string{0: "0.000000", 1: "0.000001", 3000000: "3.000000", 3737579: "3.737579", 12345678901: "12345.678901"} {
		if got := cost.FormatMicros(micros); got != want {
			t.Errorf("formatMicros(%d) = %q want %q", micros, got, want)
		}
		if back, err := cost.ParseMicros(cost.FormatMicros(micros)); err != nil || back != micros {
			t.Errorf("round trip %d -> %d %v", micros, back, err)
		}
	}
	if cost.FormatMicrosTrim(300000) != "0.3" || cost.FormatMicrosTrim(3000000) != "3" || cost.FormatMicrosTrim(1) != "0.000001" {
		t.Error("formatMicrosTrim")
	}
	if cost.MicrosToFloat(3000000) != 3 || cost.MicrosToFloat(70000) != .07 {
		t.Error("microsToFloat boundary")
	}
}

func TestMoneySumExact(t *testing.T) {
	row := &model.TaskState{}
	for i := 0; i < 10; i++ {
		for _, cost := range []float64{.1, .2} {
			if err := row.AddCost(cost); err != nil {
				t.Fatal(err)
			}
		}
	}
	if row.ReportedCostMicros != 3000000 || row.Cost != 3 || cost.FormatMicros(row.CostMicros()) != "3.000000" {
		t.Fatalf("10 x .1 + 10 x .2 = %d micros (%v)", row.ReportedCostMicros, row.Cost)
	}
	// The float sum the old code produced is not exactly 3.
	naive := 0.0
	for i := 0; i < 10; i++ {
		naive += .1
		naive += .2
	}
	if naive == 3 {
		t.Log("float sum happened to be exact on this platform")
	}
	if err := row.AddCost(-1); err == nil {
		t.Error("negative cost accepted")
	}
	if err := row.AddCost(math.NaN()); err == nil {
		t.Error("NaN cost accepted")
	}
	// A run total across active and archived rows is exact too.
	s := &model.State{Tasks: map[string]*model.TaskState{"a": {Cost: .1}, "b": {Cost: .2}}, ArchivedTasks: []model.TaskState{{Cost: .3}}}
	if got := model.AdaptiveTotalMicros(s); got != 600000 || s.ReportedCost() != .6 {
		t.Fatalf("total %d %v", got, s.ReportedCost())
	}
}

func TestMoneyOldStateCompatibility(t *testing.T) {
	var row model.TaskState
	if err := json.Unmarshal([]byte(`{"spec":{"id":"a"},"status":"verified","attempt":1,"reported_cost_usd":0.07}`), &row); err != nil {
		t.Fatal(err)
	}
	if row.ReportedCostMicros != 0 || row.CostMicros() != 70000 {
		t.Fatalf("older state without reported_cost_micros: %d", row.CostMicros())
	}
	// Derivation uses the same half-even rounding as ingest.
	row = model.TaskState{Cost: 0.0000025}
	if row.CostMicros() != 2 {
		t.Fatalf("derived %d, want 2", row.CostMicros())
	}
	// Adding to an older row folds the derived amount in and writes both fields.
	row = model.TaskState{Cost: .07}
	if err := row.AddCost(.03); err != nil || row.ReportedCostMicros != 100000 || row.Cost != .1 {
		t.Fatalf("%+v %v", row, err)
	}
	raw, _ := json.Marshal(row)
	if !strings.Contains(string(raw), `"reported_cost_usd":0.1`) || !strings.Contains(string(raw), `"reported_cost_micros":100000`) {
		t.Fatalf("both fields must be written: %s", raw)
	}
}

func TestMoneyProjectionTotalsExact(t *testing.T) {
	s := &model.State{Tasks: map[string]*model.TaskState{}}
	for i := 0; i < 10; i++ {
		s.Tasks[string(rune('a'+i))] = &model.TaskState{Cost: .1, ReportedCostMicros: 100000}
		s.Tasks[string(rune('k'+i))] = &model.TaskState{Cost: .2, ReportedCostMicros: 200000}
	}
	if got := ProjectCost(s).KnownReportedUSD; got != 3 {
		t.Fatalf("known reported = %v, want exactly 3", got)
	}
}

func TestMoneyCostExceeds(t *testing.T) {
	if cost.CostExceeds(.3, .3) || !cost.CostExceeds(.300001, .3) || cost.CostExceeds(.1+.2, .3) {
		t.Fatal("boundary")
	}
	if !cost.CostExceeds(math.NaN(), 1) || !cost.CostExceeds(1, math.NaN()) {
		t.Fatal("invalid amounts must fail closed")
	}
}

func TestMoneyProviderPoolAdmissionBoundary(t *testing.T) {
	const allocation = 1_000_000
	if !poolAdmits(400_000+100_000, 500_000, allocation) {
		t.Fatal("spent+reserved+request exactly equal to the allocation must be admitted")
	}
	if poolAdmits(400_000+100_000, 500_001, allocation) {
		t.Fatal("one micro over the allocation must be refused")
	}
	if poolAdmits(math.MaxInt64, 1, allocation) {
		t.Fatal("overflow must refuse")
	}

	p := &ProviderPool{Opts: ProviderPoolOptions{Budget: 1}, Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{
		"done":    {Status: "completed", Reserved: .1 + .2}, // older ledger: float only
		"pending": {Status: "pending", Reserved: .2},
	}}}
	// .3 (float 0.30000000000000004 in memory) + .2 reads as 500000 micros.
	if p.ReservedCost() != 500_000 || p.budgetMicros() != allocation {
		t.Fatalf("used=%d budget=%d", p.ReservedCost(), p.budgetMicros())
	}
	if !poolAdmits(p.ReservedCost(), 500_000, p.budgetMicros()) || poolAdmits(p.ReservedCost(), 500_001, p.budgetMicros()) {
		t.Fatal("ledger-backed boundary")
	}
	var r model.ProviderReceipt
	r.SetReserved(1)
	if r.Reserved != 0.000001 || r.ReservedAmount() != 1 {
		t.Fatalf("%+v", r)
	}
}
