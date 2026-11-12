# Public semantic display metadata

Plans may carry optional, producer-authored display strings. They are not verified for quality, and execution status is reported separately from them.

## Plan

`display` is `{version:1, publish, title<=60, purpose<=140}`. Any other version is rejected.

## Task

`display` is `{title<=48, why_spawned<=100, done_when<=140, outcome_field}`. Limits count runes. A non-empty `outcome_field` must name a `requires` field and must not be a `file_fields` entry. Task validation does not require `Plan.display`.

## Publication

The fields are optional and omit-empty. Validation ensures allowed lengths and schema conformity.

For `haiku`, the prompt gains `display_publication` instructions with a field mapping when a task names an outcome field. The outcome travels through the normal Result map, and there is no second response channel.

This metadata has no ETA, baseline or worker-capacity fields.

## Example

See `examples/display-plan.json`.
