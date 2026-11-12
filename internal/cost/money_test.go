package cost

import (
	"errors"
	"math"
	"testing"
)

func TestParseMicros(t *testing.T) {
	cases := []struct {
		input string
		want  int64
		err   bool
	}{
		{"0", 0, false},
		{"0.00", 0, false},
		{"-0", 0, false},
		{"1", 1_000_000, false},
		{"0.000001", 1, false},
		{"0.0000005", 0, false}, // half to even on zero -> 0
		{"0.0000015", 2, false}, // half to even on one -> 2
		{"0.25", 250_000, false},
		{"1000", 1_000_000_000, false},
		{"-0.01", 0, true},
		{"abc", 0, true},
		{"1e9", 1_000_000_000_000_000, false},
		{"2e9", 0, true}, // exceeds MaxUSD / MaxMicros
	}

	for _, c := range cases {
		got, err := ParseMicros(c.input)
		if (err != nil) != c.err {
			t.Errorf("ParseMicros(%q) err = %v, want err = %v", c.input, err, c.err)
		}
		if err == nil && got != c.want {
			t.Errorf("ParseMicros(%q) = %d, want %d", c.input, got, c.want)
		}
	}
}

func TestResolveCostPrecedence(t *testing.T) {
	// micros wins when present
	got, err := ResolveCost(500_000, 1.0)
	if err != nil || got != 500_000 {
		t.Fatalf("ResolveCost(500_000, 1.0) = %d, %v; want 500_000", got, err)
	}

	// usd used when micros == 0
	got, err = ResolveCost(0, 0.25)
	if err != nil || got != 250_000 {
		t.Fatalf("ResolveCost(0, 0.25) = %d, %v; want 250_000", got, err)
	}

	// rejects NaN, Inf
	_, err = ResolveCost(0, math.NaN())
	if err == nil {
		t.Fatal("ResolveCost allowed NaN")
	}
	_, err = ResolveCost(0, math.Inf(1))
	if err == nil {
		t.Fatal("ResolveCost allowed +Inf")
	}

	// rejects > MaxMicros
	_, err = ResolveCost(MaxMicros+1, 0)
	if err == nil {
		t.Fatal("ResolveCost allowed overflow micros")
	}
}

func TestResolveCostExplicit(t *testing.T) {
	cases := []struct {
		name    string
		micros  int64
		present bool
		usd     float64
		want    int64
		wantErr error
	}{
		{"absent micros uses usd", 0, false, 1.5, 1_500_000, nil},
		{"absent zero zero", 0, false, 0, 0, nil},
		{"absent positive micros wins", 250_000, false, 1.5, 250_000, nil},
		{"explicit zero beats positive usd", 0, true, 1.5, 0, nil},
		{"explicit zero with zero usd", 0, true, 0, 0, nil},
		{"explicit positive beats larger usd", 250_000, true, 1.5, 250_000, nil},
		{"explicit positive beats zero usd", 250_000, true, 0, 250_000, nil},
		{"explicit negative micros", -1, true, 0, 0, errNegativeMoney},
		{"explicit overflow micros", MaxMicros + 1, true, 0, 0, ErrMoneyRange},
		{"explicit zero negative usd fails closed", 0, true, -0.5, 0, errNegativeMoney},
		{"explicit zero NaN usd fails closed", 0, true, math.NaN(), 0, errInvalidMoney},
		{"explicit zero Inf usd fails closed", 0, true, math.Inf(1), 0, errInvalidMoney},
		{"explicit zero overflow usd fails closed", 0, true, maxUSD * 2, 0, ErrMoneyRange},
		{"absent negative usd", 0, false, -1, 0, errNegativeMoney},
		{"absent negative micros", -5, false, 0, 0, errNegativeMoney},
	}
	for _, c := range cases {
		got, err := ResolveCostExplicit(c.micros, c.present, c.usd)
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) {
				t.Errorf("%s: err = %v, want %v", c.name, err, c.wantErr)
			}
			if got != 0 {
				t.Errorf("%s: failed closed result = %d, want 0", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %d, %v; want %d", c.name, got, err, c.want)
		}
	}
}

func TestFormatting(t *testing.T) {
	if got := FormatMicros(250_000); got != "0.250000" {
		t.Errorf("FormatMicros(250_000) = %q, want 0.250000", got)
	}
	if got := FormatMicrosTrim(250_000); got != "0.25" {
		t.Errorf("FormatMicrosTrim(250_000) = %q, want 0.25", got)
	}
}
