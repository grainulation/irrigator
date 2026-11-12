# observer_shape contract

`observer_shape` is optional, read-only plan metadata in the canonical A x D x S notation. A is `roots`, D is `levels` (total levels including roots), and S is `max_children`. It never grants capability: it adds no spawn, depth, task, provider or context rights.

- It is checked only at initial `loadPlan` and `initialize`, never in generic `validate()`, so graph revisions are not blocked by it.
- A, D and S must be positive. D is at most 16. The ceiling is `shapeMaxNodes`, at most 1024. D=1 with any S>=1 is valid.
- `roots` must equal the plan's root task count. `levels-1` must not exceed the effective `max_depth` (default 4).
- When D>1, every root needs a spawn policy. Its effective cap (0 means 8) must be `<=` S.
- The read-only projection (`capacity`) compares the declared shape with the active graph: root count, max depth, direct child count, total task count and spawn caps.
- On any mismatch it reports `known=false` with a static `reason`. It reports no ceilings. The shape is kept as provenance only.
- Malformed persisted shape or topology (cycle, missing owner, missing row) also reports unknown. A shape that is nil or omitted gives `absent`.
- A nil shape is omitted from the plan JSON, so `PlanHash` is unchanged for legacy plans.
- Capacity is a shared allocation ceiling, not a per-parent promise or progress.
