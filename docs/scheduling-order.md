# Scheduling order

Irrigator chooses among tasks that are already dependency-ready. Ordering never changes readiness, `--workers` capacity, sizing admission, budgets, ownership barriers or task identity.

## Default: depth (unchanged)

Ready work that gates the longest unfinished dependency/ownership chain starts first. With a wide recursive plan this finishes one branch deeply before starting all roots: the retained 992-task audit (`992-order-audit.json`) started 896 descendants before the last of 32 roots (position 928). That is real behavior, not a display artifact.

## Opt-in: breadth

`--schedule breadth` stably re-sorts the ready list by ownership level (plan roots = 0, children = 1, ...), after the depth ordering, so ready roots start before ready descendants and ties keep depth order. It only applies to tasks the controller has already found ready; a task whose dependencies are not `verified`, or whose owner has not been admitted, is never started early.

- `irrigator apply PLAN --run DIR --schedule breadth ...` and `irrigator resume DIR --schedule breadth|depth`.
- Omitting `--schedule` keeps the run's saved order (default depth). Resume never silently switches.
- The choice is saved in `state.json` as `scheduling` (absent = depth, outside `plan`/`plan_hash`) and a change is journaled as one `scheduling_order` event with `detail.order`.
- `deliver` does not accept the flag; it keeps the saved order.

## Demo

`irrigator demo` explicitly selects breadth (`--schedule depth` restores depth). The credential-free equivalent through the general CLI, from the `irrigator/` directory (mock backend, no paid calls):

    irrigator apply examples/terminal-swarm.json --run /tmp/irrigator-breadth-demo --backend mock --workers 4 --schedule breadth

Breadth does not add parallelism: with fewer workers than ready roots, the run is still bounded by `--workers`.
