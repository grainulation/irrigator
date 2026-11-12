package model

const BackupCompatibilityV3 = "checkpoint version 5; offline evidence only; no migration or execution authority"

// CheckpointMigration records a storage-schema transition. The original plan,
// task interpretation and plan hash remain immutable. Its archive digest must
// match the exclusive recovery receipts before a checkpoint can be loaded.
type CheckpointMigration struct {
	Schema              string `json:"schema"`
	FromVersion         int    `json:"from_version"`
	ToVersion           int    `json:"to_version"`
	OriginalStateSHA256 string `json:"original_state_sha256"`
}

func IsRecoveryMigratedCheckpoint(s *State) bool {
	m := s.CheckpointMigration
	return m != nil && m.Schema == "irrigator.checkpoint-migration/v1" && m.FromVersion == 4 && m.ToVersion == 5 && len(m.OriginalStateSHA256) == 64 && s.Version == 5 && s.Plan.APIVersion == GovernedPlanVersion
}
