package managed

import (
	"encoding/json"

	"github.com/grainulation/irrigator/internal/cost"
)

// finiteReportedCost reads a reported cost from the raw JSON number, never
// through float64, and returns its exact micro-dollar value as USD for the wire
// structs that keep a number.
func FiniteReportedCost(value any) (float64, bool) {
	micros, ok := ReportedUSDMicros(value)
	if !ok {
		return 0, false
	}
	return cost.MicrosToFloat(micros), true
}

// reportedUSDMicros is the exact micro-dollar value of a raw cost_usd number.
func ReportedUSDMicros(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	micros, err := cost.ParseMicros(number.String())
	if err != nil {
		return 0, false
	}
	return micros, true
}
