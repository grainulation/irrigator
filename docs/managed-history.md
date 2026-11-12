# Portable accepted history for managed execution

New `swarm/v5` plans may opt a profile into `explicit_history` through
`required_capabilities`. Existing profiles, bindings, requests, and v1 dispatch
receipts retain their prior encoding. Older plan versions reject managed history
opt-in. Tools remain disabled; this capability does not grant provider tools,
credentials, additional inputs, helper access, or a vendor session.

The worker derives history from the immutable admission snapshot used for its
current attempt. It walks the main agent's exact `previous_task` chain and requires
verified predecessors with matching run/agent/index and operation/generation/attempt
identities. Missing, unaccepted, or inconsistent sources fail before dispatch,
even when an authorization boundary would exclude their content.

History contains prior accepted prompts and declared string artifacts. It excludes
undeclared result keys and the reserved `turn_request` field, which can contain
helper prompts and continuation instructions. No previous raw context maps,
provider-private reasoning, helper conversations, or provider session IDs are
copied. The current ordinary request retains its existing explicit dependency
contract; history does not alter that contract.

Every included historical source must already be an authorized dependency in the
**current** request, and all included artifact fields must match the currently
selected fields and values. An older turn's authorization never overrides a newer
selector. Ordinary generated continuations typically authorize only their direct
predecessor. The second turn can receive first-turn history; later turns may
receive no history when previous prompts used earlier inputs that are no longer
authorized. Longer history requires explicit current access to those inputs and
older accepted outputs.

The compiler walks from newest to oldest and stops at the first boundary:

- `context_boundary`: the prior prompt used a different context selection/value.
- `output_selection_boundary`: current or next-turn dependency selection excludes
  any declared public artifact or the source itself.
- `external_input_boundary`: a prior prompt had input fields (including earlier same-agent outputs) that
  are no longer available in the current authorized inputs.
- `profile_boundary`: the historical binding differs from the current binding.
- `start_of_agent`: the complete authorized chain reaches its first turn.

The returned entries are chronological, with an explicit boundary reason. The
runtime currently inherits a continuation's profile and forbids changing it within
an agent; provider-neutral history does not introduce provider switching.
Helpers receive no main-agent history, even when their inherited profile opts in.
This is conservative authority checking, not semantic information-flow or taint
tracking: currently authorized accepted artifacts can themselves contain derived
data. The compiler does not claim to detect or redact such content.

## Delivery and evidence

For opted-in main tasks, the managed adapter adds a top-level `history` member to
the existing request JSON. Existing top-level `task`, `run_id`, `operation_id`, and
other request fields keep their positions and meaning. The object uses
`irrigator.managed-history/v1` and pins the current plan, binding, run, agent, task,
operation, attempt and turn index. Each entry includes source task, operation,
generation, attempt and index, its accepted prompt, projected output, and the
projected-output SHA-256. A digest pins the complete history object.

The full submitted stdin, including history and launch context, must fit the
profile input-byte limit and the global input bound. Adapter schema/system-prompt
limits also remain authoritative. History is bounded by the existing 16-main-turn
limit and is never silently truncated to fit. Oversized requests fail before
provider execution.

Opt-in dispatch emits `irrigator.managed-dispatch/v2`, with exact stdin SHA-256,
history digest/boundary, output-schema SHA-256 when used, and Claude system-prompt
SHA-256 when used. The ordinary `.input.json` is the exact submitted stdin. Helper
receipts explicitly report `helper_isolation`, omit the history digest, and still
pin their input bytes. These are adapter submission/receipt facts, not proof of
model comprehension, exhaustive delivery after a process interruption, or vendor
identity. The existing context receipt continues to attest only its original
context/dependency scope.

A receipt persistence failure after possible execution preserves the existing
uncertain-outcome behavior; it cannot authorize automatic replay or erase unknown
cost exposure. There is no second mutable conversation ledger or checkpoint
migration. Mock and executable-fixture tests cover both managed Claude and Codex
without credentials or model calls.
