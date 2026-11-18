# Compatibility in Irrigator 1.x

Irrigator's public contracts are its documented, non-experimental command and
machine interfaces, versioned plans and worker messages, and supported saved-run
formats. The CLI version, plan `api_version`, worker `protocol`, checkpoint
`version` and storage `schema_version` are separate identifiers. Updating the
executable does not grant execution authority or consent to spend.

## Stable interfaces

Within 1.x, documented command names, flags and exit-status meanings retain their
meaning. Documented JSON result fields and MCP tool names, arguments and result
fields retain their meaning and types. New commands, optional arguments and
result fields may be added. Consumers should select documented fields and allow
additional response fields; this does not relax strict plan or request decoding.
Human-readable help, error wording, terminal layouts and ordering of incidental
output are not byte-stable APIs. Use the documented machine-readable interfaces
for automation. See [MCP](mcp.md) and [runtime notation](runtime-notation.md).

The plan loader supports `swarm/v1` through `swarm/v6`, and the omitted
`api_version` legacy form. Each version keeps its own admission and execution
rules; a new executable does not reinterpret a legacy plan as a newer version.
The current [plan schema](plan.schema.json) and runtime validator remain the
source of accepted combinations. New versions may add capabilities without
rewriting an admitted plan or its hash.

Custom providers use `swarm.worker/v1`: request identity, string-valued output
fields, outcome certainty and cost-accounting semantics remain compatible. A
provider must still implement its advertised cancellation, budget and replay
behavior. See the [worker protocol](provider-protocol.md).

## Saved runs and upgrades

Supported checkpoints are validated before use. A successful upgrade preserves
admitted plans and their hashes, accepted results, historical attempts and the
meaning of recorded uncertainty. Missing legacy identities or exact cost fields
may be derived by the documented loader; that normalization is not a new
provider invocation. Unsupported future checkpoint or storage versions fail
explicitly rather than being guessed at.

When a checkpoint needs a storage-schema upgrade, reading it does not rewrite
it. The first successful update creates or validates a non-overwriting backup
before publishing the migrated checkpoint. A failed backup prevents that
publication. Preserve the entire run directory, not just `state.json`: attempt
files, accepted artifacts, receipts and coordination files are part of its
recovery evidence. Documented artifact names and formats remain compatible;
internal temporary files are not an interchange API.

Upgrading cannot turn interrupted external work into automatic replay. Missing
legacy budget consent remains missing until an explicit operator decision;
a migration or newer schema version does not supply it. Recovery, retry and
provider-executable migration retain their explicit authority checks. See
[recovery](recovery.md), [backup](backup-restore.md), and
[provider migration](provider-migration.md).

Regression tests include a fixed synthetic pre-schema checkpoint that exercises
legacy loading, exact backup preservation and uncertainty across migration. It
represents a legacy shape, not an archived production run or proof of every
historical state combination.

## Scope and exceptions

- Features explicitly marked experimental, including opt-in messaging and replay
  fixture export, are outside this stable contract. See [experiments](experiments.md)
  and [debug fixtures](debug-workflow.md).
- Go `internal/` packages, private helpers and implementation layouts are not a
  public Go library API. See [architecture](architecture.md).
- External provider CLIs, models, authentication services and agent hosts can
  change independently. Their documented tested configurations do not promise
  compatibility with every future upstream release.
- Platform and toolchain requirements are documented in [platform support](platform-support.md).
  Native Windows and hosted or multi-host execution are outside the supported
  local runtime contract.
- Security fixes may tighten rejection of unsafe or corrupt input. They do not
  authorize rewriting immutable provenance, silently replaying uncertain work,
  or weakening spending and permission boundaries. Material compatibility
  changes belong in the changelog and upgrade guidance.
