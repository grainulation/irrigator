# Provider selection and readiness

New coding runs use the provider, model and reasoning preference supplied by
their launcher or explicitly chosen by the user. Irrigator does not guess the
conversation's model from a host name, choose Claude for a Codex-only user, or
silently replace high effort with low effort.

## Choose a single route

Explicit invocation flags take precedence, followed by the supplied launcher
preference and then an explicit saved default. If nothing selects a provider,
`irrigator code` asks for `--agent` or preferences before creating a run.
Managed init requires a model selector as well. Use the canonical selector
reported by the host/provider; a nickname such as “Sol high” is not translated
into a guessed provider ID.

```sh
irrigator init --managed --profile codex:MODEL --effort high --out new-plan.json
irrigator code --agent codex --model MODEL --effort high --repo . \
  --task 'review=Review the source changes' --plan-only
```

Replace `MODEL` with an available selector. Both commands above only prepare
plans. Executing the resulting work still uses the session's applicable model,
budget and permissions authorization. A preference file does not supply any
authorization or credentials.

Pass `--preferences FILE` to managed `init`, `code` or `profiles route`, or set
`IRRIGATOR_EXECUTION_PREFERENCES` to an explicit local file. `size` also accepts
these preferences as an explicit `PROVIDER:MODEL` reservation-calibration key;
pass `--backend NAME` to use an existing named calibration instead. It never
assumes Haiku when no route is selected. Hosts can generate:

```json
{
  "schema": "irrigator.preferences/v1",
  "launcher": {
    "provider": "codex",
    "model": "MODEL",
    "effort": "high"
  },
  "multimodel": false,
  "disabled_providers": ["claude"]
}
```

The file is bounded to 64 KiB, requires canonical fields and rejects duplicate
keys. `launcher` and `default` are optional requested tuples containing
`provider`, `model` and `effort`; `launcher` takes precedence over `default`.
An Ollama tuple may also supply `endpoint` with an explicit validated loopback
URL; doctor checks that inventory rather than assuming the default port.
Other providers cannot set an endpoint. Providers are `claude`, `codex`, `agy` or `ollama`. The managed CLI scaffold
supports the first three; Ollama execution uses a separately configured
`swarm/v6` catalog with an explicit loopback endpoint and pinned model digest.
See [local Ollama](managed-ollama.md). No server is started and no model is
downloaded by preference resolution.

Only fields supplied by the host or user should be written. An absent model
stays unknown. For explicitly selected coding agents with no model preference,
the established agent default remains available: Sonnet for Claude, or the
Codex/agy CLI's own configured default. A supplied matching launcher model and
effort take precedence over those agent defaults. Changing provider explicitly
clears the old provider's model and effort before applying overrides.

Managed CLI scaffolds retain low effort only when no effort was requested by
the user or launcher. Claude and Codex profiles support `low`, `medium` and
`high`; agy uses `low` with effort carried by its model selector. Ollama profiles
use `none`. Model-specific effort support is still a provider preflight concern.
Unsupported effort fails rather than causing a silent downgrade. New selection
provenance is retained in plan context and the model/effort remains part of the
admitted execution binding. Older admitted runs keep their original bindings
and are not rewritten on resume.

## Select multiple profiles explicitly

A new scaffold has one allowed profile, `default`. Tasks and spawned helpers
retain that profile. To use several models, prepare an execution catalog and
explicit role policy, then opt the new plan into mixed routing:

```sh
irrigator profiles route plan.json --policy roles.json --multimodel \
  --include-profiles primary,reviewer --exclude-profiles unused \
  --out routed-plan.json
```

The include/exclude names must already belong to the plan's `allowed_profiles`.
The command can narrow that set but cannot add executable grants. A task whose
chosen profile is excluded fails validation; it is never silently reassigned.
The new output plan is separate from the source and existing runs. Older
explicit role-policy workflows remain compatible; launcher-selected single-model
plans require `--multimodel` or a saved multi-model opt-in before mixed routing.

Saved preferences can contain `include_profiles`, `exclude_profiles` and
`multimodel`. CLI include choices replace the saved include list, while CLI
exclusions add to saved exclusions. `disabled_providers` applies to both single
selection and role routing and is never bypassed by an automatic fallback.
Discovery and health changes do not enroll newly installed providers. There is
no automatic cross-model or cross-provider fallback, including for uncertain
attempts; a different route needs an explicit new choice within the existing
authority rules.

## Inspect availability without generation

`irrigator agents` lists installed CLI versions and reachable Ollama inventory.
An installed CLI is not proof of login, model access, quota or successful
generation. `irrigator doctor` reads the same preference environment variable
and reports its `selection`, `selection_error` or `execution_health` separately
from credential-free local readiness.

For a selected Claude route, doctor checks bounded Claude login status. For a
selected Codex route, it checks bounded `codex login status`; raw account output
is discarded. For Ollama, inventory evidence is reported without claiming that
the model's local identity or generation has been attested. agy authentication
remains unchecked when no passive status contract is available.

`ready` and `local_ready` describe mock execution. `model_ready` describes the
selected route's passive preflight readiness. `execution_health` distinguishes
installation/reachability, authentication, requested model, model-support
evidence, selection source, generation evidence and check time. Even
`ready_for_preflight` does not establish model entitlement or quota; generation
remains `not_probed` until an explicitly authorized bounded model call runs.
Missing Claude does not fail a healthy selected Codex route.

These commands do not log in, copy credentials, start a service, install models,
or run a paid generation. A real local-model benchmark is a separate explicitly
configured operation with its own endpoint, digest and resource bounds.
