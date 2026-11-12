package model

import (
	"errors"
	"strconv"
	"strings"
)

const TurnShapeVersion = "irrigator.turn-shape/v1"
const maxTurnExecutions = 1024
const MaxMainTurns = 16

// TurnShape is distinct from the recursive Shape. Helpers are one-shot
// terminal leaves available on each main turn; they never continue or spawn.
// MainTurns includes the initial call. Each root progresses independently; no
// global turn barrier is implied. Turns never create new main-agent identities.
// All capacities are optional ceilings.
type TurnShape struct {
	Roots          int `json:"root_agents"`
	MainTurns      int `json:"max_turns_per_agent"`
	HelpersPerTurn int `json:"helpers_per_turn"`
}

type turnShapeReport struct {
	APIVersion          string    `json:"api_version"`
	PreviewOnly         bool      `json:"preview_only"`
	RequestedShape      string    `json:"requested_shape"`
	Shape               TurnShape `json:"shape"`
	LegacyOneSentinel   bool      `json:"legacy_one_sentinel"`
	MaxAgents           int       `json:"max_agents"`
	MaxMainExecutions   int       `json:"max_main_executions"`
	MaxHelperExecutions int       `json:"max_helper_executions"`
	MaxExecutions       int       `json:"max_executions"`
}

// parseTurnShape requires all three components. For shorthand compatibility,
// Z=1 means no branching, just like Z=0; Z>=2 means exactly Z helper slots per
// main turn. Canonical configuration uses explicit HelpersPerTurn (0 means none,
// 1 means one helper), rather than reinterpreting a stored numeric field.
func ParseTurnShape(input string) (turnShapeReport, error) {
	normalized := strings.NewReplacer("×", "x", "X", "x").Replace(strings.TrimSpace(input))
	parts := strings.Split(normalized, "x")
	if len(parts) != 3 {
		return turnShapeReport{}, errors.New("turn shape must be explicit XxYxZ")
	}
	values := [3]int{}
	for i, p := range parts {
		if p == "" {
			return turnShapeReport{}, errors.New("turn shape has an empty component")
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return turnShapeReport{}, errors.New("turn shape components must be decimal integers")
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return turnShapeReport{}, errors.New("turn shape component exceeds integer range")
		}
		values[i] = n
	}
	shape := TurnShape{Roots: values[0], MainTurns: values[1], HelpersPerTurn: values[2]}
	sentinel := shape.HelpersPerTurn == 1
	if sentinel {
		shape.HelpersPerTurn = 0
	}
	report, err := ReportTurnShape(shape)
	if err != nil {
		return turnShapeReport{}, err
	}
	report.RequestedShape = normalized
	report.LegacyOneSentinel = sentinel
	return report, nil
}

// reportTurnShape consumes canonical fields, not shorthand sentinel values.
// Checked divisions precede products, so even hostile platform-sized integers
// cannot overflow. A turn count is not a worker-concurrency setting.
func ReportTurnShape(shape TurnShape) (turnShapeReport, error) {
	bad := func(message string) (turnShapeReport, error) { return turnShapeReport{}, errors.New(message) }
	if shape.Roots < 1 || shape.MainTurns < 1 || shape.MainTurns > MaxMainTurns || shape.HelpersPerTurn < 0 {
		return bad("turn shape requires positive roots, 1..16 main turns and nonnegative helpers")
	}
	if shape.Roots > maxTurnExecutions/shape.MainTurns {
		return bad("turn shape exceeds 1024 executions")
	}
	main := shape.Roots * shape.MainTurns
	if shape.HelpersPerTurn > (maxTurnExecutions-main)/main {
		return bad("turn shape exceeds 1024 executions")
	}
	helper := main * shape.HelpersPerTurn
	return turnShapeReport{
		APIVersion: TurnShapeVersion, PreviewOnly: true,
		Shape: shape, MaxAgents: shape.Roots + helper, MaxMainExecutions: main, MaxHelperExecutions: helper, MaxExecutions: main + helper,
	}, nil
}
