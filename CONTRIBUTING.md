# Contributing

Irrigator accepts focused changes that preserve durable execution and make its
behavior easier to understand, verify, and operate. Describe the problem and the
observable behavior you want to change before introducing a new abstraction or
large dependency.

## Development setup

Go 1.26 or newer is the module minimum; development and CI checks pin the Go 1.27.2 toolchain. You also need `make` and a POSIX shell. Python 3
is needed for the workflow metadata checks. Third-party Go
module dependencies are allowed when they are clearly the better tool: prefer
mature, maintained libraries, pin exact versions, review what they pull in,
keep `go.mod` and `go.sum` tidy, and keep them out of the durable runtime core
where practical (for example, the review terminal UI lives in
`internal/reviewtui` so runtime packages never import Charm). Do not add one
for something trivial. The executable entry point is `main.go` at the module root; the runtime lives in `internal/<domain>`
packages (layer table in [docs/architecture.md](docs/architecture.md)), each with a `doc.go` stating its
responsibility and allowed dependencies, and its unit tests beside it. Run commands from the module root.
`make build` builds the module root into the local `irrigator` executable.

```sh
make check-fast
make build
make smoke
make demo
make check
make check-integration
make check-tools
make lint
make check-exports
make replay-gate
make acceptance
```

`make check-fast` runs every internal package's unit tests and vet for quick local
feedback. It skips the race detector and the integration tests. `make check` runs the
default (untagged) test suite under the race detector, plus vet.

**Integration tests need `-tags integration`.** Plain `go test ./...` and `make check` do
**not** run them: they sit in `internal/cli/*_integration_test.go` (plus two process-supervision
files beside their owners) behind the `integration` build tag, and they spawn workers by re-executing
the test binary. `make check-integration` runs the whole tree with `-tags integration` under the race
detector, with no `-run` filter, and `go vet -tags integration ./...`. Run a single integration test with
`go test -tags integration ./internal/cli -run TestName`.

`make lint` runs golangci-lint (the pinned version and the tuned `.golangci.yml`), `gofmt -l`
and `go mod tidy -diff`; install the linter with
`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0`. Set `LINT_NEW_FROM_REV=<commit>`
to see only findings introduced after a commit. `make check-exports` builds `tools/exportcheck` and reports
exported names in `internal/` that no other package's production code uses, and fails on any that is not
exempted (`EXPORTCHECK_STRICT=0` makes it report-only). Un-export such a name, or add it to
`tools/exportcheck/exemptions.txt` only with a reason. `make check-tools` runs the Python
tool and repository-consistency checks. `make replay-gate` (experimental) replays the
redacted failure fixtures in `internal/debug/testdata/replay` (a dev and a held-out set,
both must pass) through the real executor path; it needs no network or credentials.
`make acceptance` aggregates `check`, `check-integration`, `check-tools`, `check-exports`, `replay-gate`,
`smoke`, `demo` and `durability`, and is the full local gate (`make lint` runs in CI and needs golangci-lint installed). A fast-check pass is not release acceptance. Use focused `go test -run`
commands for the feature being changed as well.

The first-use commands run mock work without credentials. Use fresh run directories
and preserve earlier results when investigating a failure. Irrigator is a terminal
CLI: review and session views run in the terminal (`irrigator review`,
`irrigator session`).

## Keep the contracts intact

- Preserve the admitted plan and its hash. Change unfinished work through the
  supported proposal mechanism, with operation fencing and durable receipts.
- Keep accepted artifacts, original acceptance contracts, attempts, and cost
  evidence. Uncertain execution must not become an automatic replay.
- Treat `swarm/v1`, later versioned plans, `swarm.worker/v1`, and documented run
  filenames as compatibility contracts. New semantics need explicit versioning.
- Keep shape, worker concurrency, and budget separate. Tests should use mock or
  owned local fixtures; paid-provider calls require explicit authorization.
- Keep terminal review, session and MCP displays read-only and public metadata
  deliberately allowlisted.
  Do not expose prompts, credentials, private paths, or provider responses through
  a new display field by default.
- Skills must operate the real CLI under the host's permissions. They must not
  become an alternate state editor or assume a host-specific invocation tool.

[Architecture](docs/architecture.md) identifies the implementation boundaries;
[AGENTS.md](AGENTS.md) provides repository guidance for coding agents.

## Test the behavior you change

Add focused tests for meaningful invariants and regressions. For concurrency,
recovery, or billing changes, cover stale operations, duplicate delivery,
interruption, and uncertainty where relevant. Run the race suite before proposing
runtime changes. Keep tests independent of a developer's home directory, accounts,
installed models, and surrounding workspace.

For platform changes, distinguish cross-compilation from actual execution and
native execution from emulation. Record the toolchain, OS/architecture, commands,
and limitations in the pull request. Terminal rendering and host-agent integration
need their own checks; a Go test pass does not establish visual acceptance or that
a host loaded a skill.

## Submit a focused pull request

Explain the concrete trigger, the resulting behavior, and the checks you ran.
Include compatibility or migration effects and material limitations. Keep generated
binaries, run state, credentials, local logs, caches, and private screenshots out of
the change. Use a mock fixture or a minimal redacted reproduction when reporting
an issue.

The project uses the [MIT license](LICENSE). Contributions should be compatible
with it; preserve notices for third-party code. Do not copy vendored dependencies
or generated build output into source without an explicit maintenance plan.

For a security issue, follow [SECURITY.md](SECURITY.md) rather than posting sensitive
reproduction details in a public issue.

Follow the [community conduct guidelines](CODE_OF_CONDUCT.md). Use
`irrigator help --json` and `irrigator schema` to inspect the generated command
and plan contracts; their checked-in snapshots have drift tests.
