package model

// WorkerBoundaries describes the admission snapshot, not a reservation. Parallel
// admissions may consume capacity; the runtime rechecks every proposed task.
type WorkerBoundaries struct {
	Topology               string `json:"topology"`
	TaskSlotsRemaining     int    `json:"task_slots_remaining"`
	CurrentDepth           int    `json:"current_depth,omitempty"`
	RemainingDepth         int    `json:"remaining_depth"`
	SpawnChildrenRemaining int    `json:"spawn_children_remaining"`
	CurrentTurn            int    `json:"current_turn,omitempty"`
	RemainingTurns         int    `json:"remaining_turns"`
	HelpersRemaining       int    `json:"helpers_remaining"`
	CanWaitForHelpers      bool   `json:"can_wait_for_helpers"`
	Basis                  string `json:"basis"`
}
