package model

import (
	"errors"

	"github.com/grainulation/irrigator/internal/cost"
)

func (t *TaskState) CostMicros() int64 {
	if t.ReportedCostMicros != 0 {
		return t.ReportedCostMicros
	}
	m, _ := cost.MicrosFromFloat(t.Cost)
	return m
}

func (t *TaskState) AddCost(f float64) error {
	m, ok := cost.MicrosFromFloat(f)
	if !ok {
		return errors.New("invalid reported cost")
	}
	sum, ok := cost.Add(t.CostMicros(), m)
	if !ok || sum > cost.MaxMicros {
		return cost.ErrMoneyRange
	}
	t.ReportedCostMicros, t.Cost = sum, cost.MicrosToFloat(sum)
	return nil
}
