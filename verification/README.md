# Verification

These checks use mock workers, fake executables and temporary repositories. They
require no model credentials and make no paid provider calls. Go 1.26 or newer is
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

## Reporting evidence

Record the exact source revision or working-tree digest, toolchain and platform,
commands executed, results and skipped layers. A cross-build does not establish
runtime support, and mock scheduling does not establish model quality or speedup.
Paid provider acceptance requires a separate explicit aggregate budget.
