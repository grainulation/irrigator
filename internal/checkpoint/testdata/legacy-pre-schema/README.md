# Synthetic legacy checkpoint

This fixed test input represents a pre-storage-schema `version: 1` checkpoint.
It was authored for upgrade regression testing; it is not a production capture
or a file attributed to a historical release. It intentionally omits
`schema_version`, `run_id`, call limits, budget-consent decisions, generation,
operation identity and exact micro-dollar fields.

The accepted draft completed on attempt two. Its dependent publish attempt is
uncertain and must remain uncertain through loading and storage migration. The
plan hash pins the compact Go JSON representation of the admitted plan.
