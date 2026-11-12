# Offline execution profiles

`profiles` validates local configuration and resolves an immutable JSON binding.
It does not start a task, look up credentials, contact a provider, install a
plugin, or change a plan/checkpoint. Existing `Plan.Providers` / `CommandSpec`
and old runs are unchanged. Managed execution is available through explicit
`swarm/v3` or `swarm/v4` plans, described below; resolving alone does not authorize dispatch.

Create a catalog at a clean absolute path (no symlink components):

```json
{
  "schema": "irrigator.execution/v1",
  "adapters": {
    "local-mock": {"kind": "mock", "protocol": "swarm.worker/v1"}
  },
  "instances": {
    "local": {"adapter": "local-mock", "credential_refs": []}
  },
  "profiles": {
    "test": {
      "instance": "local",
      "model": "mock",
      "effort": "none",
      "required_capabilities": ["structured_strings", "outcome_certainty"],
      "limits": {"input_bytes": 65536, "output_bytes": 32768},
      "tools": [],
      "operation_budget_usd": "0"
    }
  },
  "default_profile": "test"
}
```

```sh
irrigator profiles inspect --catalog /absolute/catalog.json
irrigator profiles resolve --catalog /absolute/catalog.json --profile test
```

`inspect` validates the entire catalog and returns its canonical digest without
checking executable bytes. `resolve` uses the named profile, or `default_profile`
when omitted, and additionally hashes the selected adapter executable. Resolving
from another working directory gives the same binding. Both commands are read-only.

For the current Claude CLI contract, declare adapter kind `claude_cli`, protocol
`swarm.worker/v1`, `command: ["/absolute/real/claude"]`, and its lowercase
`executable_sha256`. There must be exactly one executable path; arbitrary command
arguments are not supported. Use a real regular executable file, not a symlink.
`resolve` opens it through pinned directory handles and hashes at most 512 MiB;
it never executes it. Files may change later: future dispatch must reverify.

A Claude instance has exactly one explicit credential reference, either
`{"kind":"host_session","name":"claude"}` or
`{"kind":"env","name":"MY_EXPLICIT_SECRET_REFERENCE"}`. These are references
only. No environment value is read, no authentication is attempted, and the
reference does not grant access or declare an env-to-vendor mapping. A future
adapter must implement that mapping explicitly. Literal credential values and
unknown fields are rejected. Mock instances cannot have credential references.

Profiles may set `timeout_seconds` to an integer from 1 to 3600. It is pinned
with the binding and applies to both the worker deadline and provider request;
when omitted or zero, the run's saved `--timeout` applies. For example, set
`"timeout_seconds": 600` for a longer Sonnet or Opus task. Changing the profile
requires a new admission; it does not rewrite an existing run.

Claude profiles accept a requested model selector, an effort of `low`, `medium`
or `high`, a positive decimal operation budget, and `tools: []`. The model is
not discovered or verified: output states `model_resolution: requested_only`,
`actual_model: null`, and `auth_verified: false`. Model availability, actual
limits, resolved aliases and valid effort support still need future live adapter
negotiation. This offline result is not proof the vendor supports the selection.

The fixed declared capability sets are `structured_strings` and
`outcome_certainty`, plus `explicit_history` for the Claude contract. Unknown
required capabilities, including token counting or pinned-model resolution, fail.
Capabilities are labeled `declared_contract_not_live_probe`. They do not certify
an arbitrary executable merely because its bytes match the supplied digest.

All profiles require explicit empty tools and positive byte limits bounded by
1 MiB input and 4 MiB output. Bytes are not model tokens. Unsupported tools,
limits, efforts and unknown fields fail rather than being silently dropped.
Mock profiles require model `mock`, effort `none`, and zero budget. Budgets are
nonnegative decimal USD strings with up to six fractional digits, converted
exactly to integer micros; negative, exponent, nonfinite and oversized values
fail. The maximum representable policy amount is $1,000,000. A declared budget
is neither spending authorization nor a reservation.

The catalog digest covers canonical decoded configuration. Binding digest covers
the catalog identity/path, selected adapter/instance/profile, declared capability
set, credential reference names and effective limits/budget. Its `digest` field
is empty while computing SHA256 over Go's JSON encoding. Neither digest is a
signature or a permission grant. Managed admission persists bindings transactionally; `profiles resolve` only
prints them.

The shared pinned-file reader is part of the complete runtime (Go 1.26.7+, the
module minimum) and is used to open catalogs. Catalogs are capped
at 256 KiB with bounded entry counts. Duplicate/case-aliased keys, omitted required
fields, nulls, trailing objects, nonregular files and symlink paths are rejected.

## Best-effort cost tracking

New `init --managed` plans use `execution.cost_policy: "track"`. A valid,
completed result can succeed when monetary cost is unavailable. Reported estimates
and unknown-cost operations remain in receipts, but tracking does not hold funds
or block admission. `run_budget_usd` is a warning threshold in this mode; zero
means no warning threshold. Supported provider per-call limits still apply.

Existing plans retain their admitted behavior: an omitted policy in v3 and
`retain_unknown` in v4 and later enforce their run budgets and retain unknown
reservations. Do not edit an admitted plan to switch modes. Create a new plan
with `track` when that behavior is desired. Campaign grants remain enforced and
reject tracking plans rather than silently weakening their spending authority.

Execution uncertainty is independent of cost: interrupted processes, ambiguous
results, and unsafe retries still require recovery. Missing cost alone is not
proof of an incomplete call. Tracking is not a provider-account spending limit and does not override a
provider service's independently configured budget.

Monetary fields ending in `_usd` are explicitly **USD**, not the user's local
currency. They contain provider-reported estimates, not verified charges. Token
usage and execution duration are separate measurements; neither is converted to
money or combined into a universal effort score. No exchange rates are applied.
Externally reconciled billing remains separately identified.

## Managed execution

`examples/managed-profiles-v3.json` runs two named mock profiles through two root
agents, two turns per agent and two terminal helpers per turn (12 executions).
It costs nothing and exercises actual child expansion and profile inheritance:

```sh
irrigator apply examples/managed-profiles-v3.json --run /absolute/new-run --workers 2
```

A managed plan embeds `execution.catalog`, an explicit `allowed_profiles` list
and `run_budget_usd`. Each task may select `profile`; otherwise the catalog's
default applies. Admission pins complete bindings and writes durable checkpoint
state. Managed plans forbid provider command flags,
task effort overrides and command validators. Explicit `--budget` is rejected
on apply/resume: operation limits come from profiles and the run ceiling comes
from the plan. Backend defaults to `managed`; a conflicting explicit
backend is rejected. Helpers and continuations retain their owner's profile.

The managed Claude adapter uses a fresh one-shot CLI session, the pinned model
selector/effort, no tools, strict empty MCP configuration and no persisted session.
It accepts only the explicit `host_session: claude` credential reference. Env
mapping is not implemented. Plans can explicitly opt into [accepted managed history](managed-history.md);
explicit selected context and dependency artifacts remain available.
There is no fallback or hidden session portability.

Every request delivers current run, task, operation and attempt identity with a
validated launch-context envelope. The input limit includes the serialized
request, output schema and system prompt; output is globally bounded by the
supervisor and rejected if above the profile limit. Executable bytes are checked
again immediately before invocation. This is not an atomic exec-by-descriptor
security boundary against concurrent modifications by the same operating-system
user. Model selectors remain requested values, not proof of actual model identity.

For enforced v3 plans, a successful paid result requires finite nonnegative
reported cost. Missing cost,
malformed output or an interrupted process remains uncertain and retains its
reservation. Positive prelaunch failure releases the reservation. Reported vendor
overshoot is recorded and stops additional admission; a local ledger cannot force
a vendor to honor its cap. The graph-wide fixed-point ledger and immutable binding
are committed with cell transitions; profile resolution itself reserves nothing.


Each dispatched attempt records `.context.json` (adapter-reported context receipt)
and `.managed.json` (binding and request identity, requested model, provider-reported
model/usage when present, reported cost or null when unknown, and certainty).
These attest delivery to the local adapter, not model comprehension. A missing
actual-model field stays null; per-model usage maps are not collapsed into a
single asserted model. Failure to persist receipts forces uncertainty. Mock
attempts also save their exact `.input.json` request.

## Codex with an existing CLI login (`swarm/v4`)

`codex_cli` uses the installed Codex executable and its existing host login.
Pin the real executable file and its SHA256, not a shell wrapper or a symlink.
The instance credential reference is `{"kind":"host_session","name":"codex"}`;
no API key is required or copied. The current adapter accepts `low` effort and
empty tools only. A model such as `gpt-6-luna` remains a requested selector;
account availability and actual model identity are not inferred from the name.

Codex plans must use `api_version: "swarm/v4"` and
`execution.cost_policy: "track"` (or `"retain_unknown"` for enforced budgets). They write checkpoint version 3 so
older binaries cannot silently misinterpret token-only accounting. The existing
`swarm/v3` / checkpoint 2 contract rejects Codex profiles.

The CLI runs in a fresh private temporary directory outside the workspace, with
user config and execpolicy rules ignored, project instructions disabled, a
read-only sandbox and shell, apps, plugins, hooks, browsing, image, skill and
subagent tools disabled. No custom provider or endpoint is installed. The host
auth store remains unchanged; inherited API-key overrides are excluded. The
explicit WorkRequest and generated output schema carry task inputs. Profile
byte limits bound those supplied inputs and returned CLI output; they do not
measure Codex's own built-in system instructions or model token limits.

Acceptance requires one terminal success, one valid final structured message
and nonnegative token usage. Tool invocations, malformed/duplicate events,
missing usage, failed turns and ambiguous cancellation remain uncertain. Known
pre-turn CLI configuration notices are diagnostics, not evidence of a failed
or successful model turn. They cannot substitute for terminal success.

Codex CLI token usage is not authoritative USD cost. A valid task may verify
while `cost_unknown` remains true. In `track` mode it holds no funds. With
`retain_unknown`, its full operation reservation stays held.
The run-local ledger cannot fund another operation from that hold, and manual
output reconciliation does not infer a dollar amount or release it. There is
no vendor-enforced USD cap in this adapter, no guessed token pricing and no
claim of zero-cost execution. Time, supplied-input size and output size remain
bounded; unbounded connection retries are disabled. The built-in login provider may
retry transport internally within the operation timeout; this CLI version rejects
per-provider retry overrides for its built-in provider. One Irrigator operation
starts one CLI invocation; that is not a guarantee of exactly one provider
request. Irrigator does not automatically replay ambiguous paid operations. This is a run-local ledger,
not a project-wide or cross-application billing ledger.


## Mixed Claude and Codex example

This creates a two-root, one-turn plan with no helpers: Claude handles one task
and Codex handles the other. Both use existing CLI logins; no API key is read or
written. Authenticate each CLI through its normal login flow beforehand. The
example requests `haiku` and `gpt-6-luna`; change those selectors to models your
accounts support. Authentication and actual model identity are not established
by validation. The Claude adapter preserves the local `USER` identity needed
for its macOS login lookup.

Run this from a private directory with Python 3 and both CLIs on PATH. It resolves
symlinks to their real executable files and pins their current bytes. The output
file must be new. This step performs no model call:

```sh
python3 - <<'PYTHON'
import hashlib, json, pathlib, shutil
adapters, instances, profiles = {}, {}, {}
for name, model in (("claude", "haiku"), ("codex", "gpt-6-luna")):
    found = shutil.which(name)
    if not found:
        raise SystemExit(name + " executable not found on PATH")
    executable = pathlib.Path(found).resolve(strict=True)
    adapters[name] = {
        "kind": name + "_cli", "protocol": "swarm.worker/v1",
        "command": [str(executable)],
        "executable_sha256": hashlib.sha256(executable.read_bytes()).hexdigest()
    }
    instances[name] = {"adapter": name, "credential_refs": [
        {"kind": "host_session", "name": name}]}
    profiles[name] = {
        "instance": name, "model": model, "effort": "low",
        "required_capabilities": ["structured_strings", "outcome_certainty"],
        "limits": {"input_bytes": 65536, "output_bytes": 65536},
        "tools": [], "operation_budget_usd": "0.25"
    }
plan = {
    "api_version": "swarm/v4", "name": "Two existing CLI logins",
    "turn_shape": {"root_agents": 2, "max_turns_per_agent": 1,
                   "helpers_per_turn": 0}, "max_tasks": 2,
    "execution": {
        "catalog": {"schema": "irrigator.execution/v1", "adapters": adapters,
                    "instances": instances, "profiles": profiles,
                    "default_profile": "claude"},
        "allowed_profiles": ["claude", "codex"],
        "run_budget_usd": "0.50", "cost_policy": "retain_unknown"
    },
    "tasks": [{"id": name, "profile": name,
               "prompt": "Return the word ready in answer.",
               "requires": ["answer"],
               "turn": {"agent_id": name, "role": "main", "index": 1}}
              for name in ("claude", "codex")]
}
with open("mixed-v4.json", "x") as output:
    json.dump(plan, output, indent=2)
PYTHON
irrigator plan mixed-v4.json
```

The example declares $0.25 per operation and $0.50 in total admission allowance.
Those are illustrative policy amounts, not spending authorization or an invoice
cap. Only after authorizing the two operations and their model selections, run:

```sh
irrigator apply mixed-v4.json --run /absolute/new-mixed-run --workers 2
irrigator status /absolute/new-mixed-run --json
```

Do not add `--budget`, external provider commands or `--backend mock` to this
managed plan. For a free check use `examples/managed-profiles-v3.json` instead.
If a pinned executable changes, regenerate a new plan before admission; never
edit an admitted plan to replace its binding.

With `retain_unknown`, a verified Codex result still leaves its USD 0.25
reservation held when USD cost is unknown. With `track`, no funds are held. Token usage is not converted to dollars. Report known spend, held
reservations and unknown cost separately; a displayed known total of zero does
not mean free execution. Missing or malformed usage/output remains uncertain
and does not authorize replay. Claude's reported cost, when present, is recorded;
`retain_unknown` never fabricates a price for either vendor. Admission checks
cannot guarantee a final vendor invoice, and no cross-run billing cap is implied.


## Provider CLI updates and compatibility checks

A paused run keeps its admitted executable hash and path. If a CLI updater removes
that version, resume fails closed. Preserve the pinned executable while a run is
active; a newly generated plan may select the updated CLI for a new run. For eligible paused runs, [explicit provider migration](provider-migration.md)
prepares a reviewed replacement binding for pending and future work while retaining
historical bindings and receipts. It never silently edits the admitted plan.
Campaign-bound and explicit-history profiles are currently excluded. A matching
model name does not establish identical executable behavior.

The automated suite uses controlled provider processes to test error responses,
protocol drift, timeouts, cleanup, and billing. It does not establish that a
vendor's latest CLI works. Before adopting a CLI update, create a fresh synthetic
managed plan with `init --managed --profile claude:haiku`, review its tracking threshold and provider per-call limit, and run it with existing authentication. Retain its receipts locally.
Do not put account logins or real project prompts into public CI. Authenticated
vendor canaries require a separately configured runner and spending authority.

Command providers can optionally run in a container; see [Container sandbox](sandbox.md).
