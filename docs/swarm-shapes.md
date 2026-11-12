# Swarm shapes

> **Agent-tree topology**: describes recursive tree expansion (A×D×S). For sequential turns and terminal helpers, see [turn shapes](turn-shapes.md).

Use **A×D×S** (or **AxDxS**) to describe a bounded recursive swarm:

- **A — agents:** number of root agents started by the coordinator.
- **D — depth:** total agent levels, including those roots as level one.
- **S — subagents:** maximum children each node may spawn.

The coordinating agent is outside this count. All three values are positive
integers. Two-dimensional **W×D** remains shorthand for **W×D×1**: each branch
continues through at most one child per level.

For a fully populated tree, the maximum node count is:

```text
A × (1 + S + S² + … + S^(D−1))

S = 1: A × D
S > 1: A × (S^D − 1) / (S − 1)
```

| Shape | Roots | Levels including roots | Maximum children per node | Maximum total agents |
| --- | ---: | ---: | ---: | ---: |
| 2×5 (2×5×1) | 2 | 5 | 1 | 10 |
| 5×6 (5×6×1) | 5 | 6 | 1 | 30 |
| 10×2 (10×2×1) | 10 | 2 | 1 | 20 |
| 10×2×9 | 10 | 2 | 9 | 100 |
| **25×5×2** | **25** | **5** | **2** | **775** |

A 25×5×2 tree can contain 25, 50, 100, 200, and 400 agents at successive
levels: **775 total, not 125**. Its roots and non-leaf descendants each spawn
at most two children. The limit permits fewer children; report whether the
run targets the fully populated tree or a smaller realized graph.

**Root count does not set concurrency.** Choose `--workers` separately for the
provider, available resources, latency, and spending cap. Descendants can run
concurrently across and within branches; ancestors waiting on children do not
occupy worker capacity. Twenty-five roots do not imply a 25-worker limit or
775 simultaneous calls.

Expand the notation into an explicit plan with A roots, a `spawn.max_children`
limit of S on eligible nodes, a bounded `max_tasks`, and `max_depth` of D−1
(plan roots have depth zero). Leaves must not spawn. For a full 25×5×2 tree,
set `max_tasks` to 775 and `max_depth` to four, subject to runtime limits.
A smaller task limit or exhausted spending cap may prevent full expansion;
never silently report that smaller graph as a completed full tree. Request
actual child manifests from agents when testing recursive delegation: a static
list of tasks does not demonstrate agent spawning.

The `irrigator size --shape A×D×S --sizing FILE` dry run parses this convention
for [cost and worker guidance](sizing.md#shape-sizing-axdxs). It does not create a plan, launch
agents, or promise to spend whatever full expansion requires. Before a paid run, state
roots, levels, branching limit, maximum and intended node counts, worker
concurrency, per-call limit, and the enforced aggregate spending cap. Per-call CLI limits alone do not enforce a graph-wide budget.

Afterward, report actual nodes and model calls, retries, observed levels and
branching, completion, and reported cost. Extra reviews and repair calls count
against spend separately; they do not retroactively change the declared tree.

For example, ten roots with up to nine children each form a **10×2×9** tree
with a maximum of 100 nodes. A **10×2×1** tree permits only 20 nodes.

## Recursive child manifests

Spawn permission is per node; children do not inherit it. A child intended to
create grandchildren must itself include `spawn` and require that output field.
A leaf has no spawn policy. Output from a leaf is retained as task data, but
cannot create runnable tasks—even if a field is named `children`. Shape limits
are ceilings, not promises that the model will populate every level.

Each child manifest needs `id`, `prompt`, and a nonempty `requires` array.
Use local IDs containing letters, digits, hyphens, or underscores, such as
`trust-c1`; do not include dots or `__`. Irrigator adds the parent namespace.
Include the intended non-leaf spawn policy explicitly in prompts and inspect
realized node counts by level after execution. Never grant spawn permission
implicitly just because a model emitted a child-like JSON object.

Built-in model workers receive an `execution_boundaries` admission snapshot with
current depth, remaining depth, remaining per-node spawn capacity, and remaining
run task slots. These are upper bounds, not reservations: parallel admissions can
consume capacity before a proposal returns. A node at the depth limit receives a
schema allowing only an empty child list; children created one level before the
limit are constrained to have no spawn policy. The runtime still validates all
proposals and rejects over-limit work instead of silently truncating it. Existing
custom command-provider request fields remain unchanged.
