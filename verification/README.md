# Verification

These checks use mock workers, fake executables and temporary repositories. They
require no model credentials and make no paid provider calls. Go 1.26.7 or newer is
required; the module selects the pinned Go toolchain when automatic toolchain
selection is enabled.

## Local acceptance

```sh
make build
make smoke
make demo
make check
npm ci --prefix examples/acp --ignore-scripts
npm test --prefix examples/acp
python3 -m unittest discover -s tools -p 'test_*.py'
python3 verification/package-safety-test.py
python3 -I verification/plugin-validator-regression.py
python3 verification/ci-platform-picker-test.py
IRRIGATOR_TEST_BINARY="$PWD/irrigator" python3 -I verification/monitoring-sensor-test.py
```

The comprehensive Go gate must include the process integration suites as well as
race detection and static analysis. See the Makefile for the current targets;
`go test -short ./...` intentionally excludes slow cases and integration-tagged
files and cannot replace that gate.

The ACP tests run local fake HTTP/agent processes. A restricted host must allow
localhost listeners for those checks; a sandbox refusal is not a provider failure.

## Verification suite matrix

| Gate | Needs | Scope | Expected outcome | Command |
| --- | --- | --- | --- | --- |
| `build` | Go 1.26.7+ | Compile `./irrigator` with `-trimpath` | Exit `0`; `./irrigator version` prints `1.1.0` | `make build` |
| `check-fast` | Go 1.27.2, `tmux` ≥3.2 | Unit tests across `./internal/...` + `go vet` | Exit `0` | `make check-fast` |
| `check` | Go 1.27.2, `tmux` ≥3.2 | Default suite under `-race` + `go vet` | Exit `0` | `make check` |
| `check-integration` | Go 1.27.2, `tmux` ≥3.2 | Full tree with `-tags integration -race -count=1` | Exit `0` | `make check-integration` |
| `check-tools` | Python 3 | Packaging, archive verification, and repo consistency | Exit `0`; unittest reports `OK` | `make check-tools` |
| `check-homebrew` | Homebrew, Go 1.26.7+ | Actual source formula recipe and mock demo/export in a private prefix; no installed keg | Exit `0`; `PASS: Homebrew head recipe` | `make check-homebrew` |
| Stable Homebrew | Homebrew, host-native release archive | Actual generated formula checksum, binary installation and version/mock demo/export; private prefix, no installed keg | Exit `0`; `PASS: Homebrew stable recipe` | `python3 -I verification/check_homebrew.py --formula FORMULA --archive ARCHIVE` |
| `check-exports` | Go 1.27.2 | Strict minimal exported surface check on `internal/` | Exit `0` | `make check-exports` |
| `replay-gate` | POSIX shell, Go | Redacted failure fixtures replay to recorded states | Exit `0` (no network/spend) | `make replay-gate` |
| `smoke` | `./irrigator` | Private `HOME` and registry; local mock `init` → `plan` → `prepare` → `resume` → `status` → `export` → `graph` → `doctor` | Exit `0`; `First-use smoke passed.` | `make smoke` |
| `demo` | `./irrigator` | Adaptive revision and recovery with mock workers | Exit `0`; `4/4 verified` | `make demo` |
| `durability` | Python 3, `./irrigator` | Stop mid-swarm and resume without repeating verified work | Exit `0`; `PASS:` | `make durability` |
| `lint` | `golangci-lint` v2.15.0 | Static analysis (default + `integration` tags), `gofmt`, `go mod tidy -diff` | Exit `0`; `0 issues.` | `make lint` |

## Regression boundaries

- GC only prunes obsolete attempt logs from a validated stopped run. It preserves
  worktrees, branches and unknown process/effect evidence for explicit recovery.
- Supervisor and controller tests must use real child processes when asserting
  process-death behavior. Closing a file handle is not a SIGKILL experiment.
- Display tests cover control characters, Unicode formatting and line separators,
  invalid UTF-8, literal escape markers, and CRLF diffs. Raw state and Git content
  remain unchanged by display sanitization.
- State migration tests retain original evidence, validate backup completeness,
  and distinguish schema version from consent to unbudgeted legacy execution.
- CLI goldens represent deliberate command contracts. Regenerate only after
  reviewing a real contract change; an unused snapshot does not establish
  compatibility.
- Homebrew acceptance must resolve the owned temporary tap's trusted formula
  name and confirm its loaded path. Direct Ruby path loading is unsupported by
  current Homebrew. The private-prefix recipe check does not prove a fresh
  network installation, dependency installation or migration of an existing keg.
- A stable formula update requires all four downloaded release archives, their
  complete matching published `SHA256SUMS`, and a common frozen source identity.
  See the [promotion instructions](../docs/homebrew.md). A release page without
  binary assets is insufficient; hashes alone do not authenticate a publisher.

## Reporting evidence

Record the exact source revision or working-tree digest, toolchain and platform,
commands executed, results and skipped layers. A cross-build does not establish
runtime support, and mock scheduling does not establish model quality or speedup.
Paid provider acceptance requires a separate explicit aggregate budget.
