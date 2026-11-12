package model

import (
	"github.com/grainulation/irrigator/internal/cost"
)

// adaptiveTotalMicros sums each recorded TaskState once across active and
// archived rows, exactly, so the result never depends on map iteration order.
func AdaptiveTotalMicros(s *State) int64 {
	var total int64
	for _, row := range s.Tasks {
		if row != nil {
			total += row.CostMicros()
		}
	}
	for i := range s.ArchivedTasks {
		total += s.ArchivedTasks[i].CostMicros()
	}
	return total
}

// adaptiveTotalCost is the JSON-boundary form of adaptiveTotalMicros.
func AdaptiveTotalCost(s *State) float64 { return cost.MicrosToFloat(AdaptiveTotalMicros(s)) }
