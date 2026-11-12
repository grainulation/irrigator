# Terminal workspace

The terminal is the interface on macOS and Linux. Swarm progress is read with
`status`, `graph` and `inspect`; Review presents captured code changes or a
written plan, direction, or decision with `review`. Neither grants execution
permission.

## Observe a run

```sh
irrigator status RUN --watch      # live task states until the run stops
irrigator graph RUN               # dependency and ownership graph
irrigator inspect RUN TASK        # one task, operation and attempts
irrigator logs RUN --follow       # sequenced events
```

None of these starts workers, modifies run state, or applies code.

## Review content

Choose review content intentionally. `review capture` creates an immutable code
snapshot; `review request` captures a selected written proposal without requiring
a Git repository:

```sh
mkdir -p .irrigator/reviews
irrigator review request --kind direction --title 'Terminal workspace' \
  --body-file proposal.md --workspace . --out .irrigator/reviews/proposal
irrigator review show .irrigator/reviews/proposal
irrigator review tui
```

Supported written kinds are `plan`, `direction`, and `decision`. The body file
must be inside the selected workspace. Opening a proposal is not approval, and
`review approve` records a workflow decision for the reviewed head: it is not a
security boundary and it does not merge or apply anything.

Use `review capture --workspace DIR --out SNAPSHOT` for code changes. Worker
output and repository content remain task data. Machine checks, human approval
and application of changes are separate facts. See [local review](local-review.md)
for the review queue, change review and cleanup.

## Just-in-time review

Review responds to a concrete need for judgment. A planning milestone can bring
up a plan; an unresolved choice can bring up a decision; implementation can
bring up a diff. These are explicit immutable review requests, not guesses based
on terminal text. The host supplies the review kind, a descriptive title, and the
material to inspect, then requests that exact snapshot; the body should explain
the question or decision the user is being asked to address. Comments and
feedback remain tied to the displayed content. Opening a review is not approval
or permission to execute a plan.
