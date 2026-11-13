# Launch and worker context

`irrigator context create` produces an immutable `irrigator.launch/v1` capsule for one launch. It contains a random session ID, canonical workspace and registry paths, the exact runtime SHA-256/version, and a headless capability. It contains no observer credential, provider secret, prompt, or copied conversation. It is routing metadata, **not** a permission grant or signed attestation.

```sh
mkdir -p "$PWD/.irrigator"
irrigator context create --workspace "$PWD" --out "$PWD/.irrigator/launch.json"
export IRRIGATOR_LAUNCH_CONTEXT="$PWD/.irrigator/launch.json"
export IRRIGATOR_SESSION_REGISTRY="$PWD/.irrigator/current-session.json"
irrigator context inspect --file "$IRRIGATOR_LAUNCH_CONTEXT"
```

Choose a new capsule filename for a new launch; creation refuses to overwrite one. `create` writes mode 0600. Paths must be clean absolute paths without symlink components; on macOS use canonical `/private/tmp`, not `/tmp`. The workspace must exist, and the registry and run destination must be inside it. Existing workflows without `IRRIGATOR_LAUNCH_CONTEXT` keep their previous behavior.

`apply`, `prepare`, `resume`, and `deliver` validate an inherited capsule before admission: its runtime must match the executing binary, the run destination must be inside its workspace, and an explicit or inherited registry must match the capsule. A changed working directory does not change these absolute identities. A replacement runtime needs a newly generated capsule. Capsule parsing is bounded to 16 KiB and rejects unknown/duplicate fields, trailing objects, symlinks found during path validation, and nonregular files. Capsule creation and reading use a pinned directory-handle walk, compare opened directory identities, and open the final component with `O_NOFOLLOW`. Replacing an ancestor with a symlink cannot redirect a pinned traversal. Capsule operations are part of the complete runtime, which needs Go 1.26.7 or later (the `go.mod` minimum); the development and CI toolchain is pinned to Go 1.27.2. Capsules contain no secrets and confer no authority. The operator or a supported host wrapper creates the capsule manually (as above) and exports the two variables into the launched agent's environment. No observer credentials belong in a capsule.

## Provider context and receipts

Actual admitted external-worker requests carry optional `launch_context` metadata under the existing `swarm.worker/v1` protocol. This adds:

- Actual run, task, parent, operation, attempt, request and task/validation-phase identity.
- Versioned turn identity when the admitted task has one, without translating ownership depth into a turn.
- The optional launch capsule, sorted selected context keys, and a hash of the selected context/dependency inputs.
- A canonical digest binding the envelope to that exact request.

The existing `Task.Context` and `input_fields` allowlists still determine content. This envelope does not widen them. Worker requests remain bounded by the existing 1 MiB input limit.

An aware provider can return:

```json
{"context_receipt":{"schema":"irrigator.context-receipt/v1","digest":"EXACT_REQUEST_CONTEXT_DIGEST","consumer":"provider-name"}}
```

The caller validates that acknowledgement before accepting the result and writes `cells/TASK/ATTEMPT.context.json` beside the normal receipts. The file contains identity and `provider_reported_consumption`, not prompt/result values. Missing acknowledgements preserve legacy compatibility and produce **no consumption claim**. Mismatched acknowledgements are malformed responses and follow existing uncertain-outcome handling; they never authorize replay.

The built-in provider adapter validates the envelope and reports its own forwarding of it after a response arrives. This does not prove that a model understood or obeyed the context. Cached results do not acknowledge a different attempt's context: operation deduplication excludes per-attempt delivery metadata, and a new-attempt cache hit omits the prior context acknowledgement. Task/context/dependency execution semantics remain bound by the existing operation payload hash.

## Verified scope

The launch-context tests (unit tests in `internal/executor`, integration tests in `internal/cli` run with `-tags integration`) exercise a real local provider subprocess and actual dynamically spawned child. It verifies selected context, parent/run/operation identity, capsule receipt binding, changed-cwd headless execution and no repeated dispatch on resume. It also rejects malformed capsules and conflicting registries before creating run state, and tests provider-cache reuse without fabricated current-attempt consumption.

These checks use no model, account, observer, or network service. They establish runtime wiring and provider-reported consumption. They do not establish that a Codex/Claude plugin is installed, that the host loaded or invoked a skill, or that native host-owned subagents are registered as Irrigator workers. Host invocation requires its own evidence.
