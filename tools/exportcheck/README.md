# exportcheck

Keeps the exported surface of `internal/...` packages minimal. Policy: an exported name in an
internal package is justified only if some OTHER package's production (non-`_test.go`) code
references it, or it is exempt. `staticcheck` U1000 cannot enforce this (whole-program
exported-name analysis was removed), hence this checker.

Single `package main`; stdlib + `golang.org/x/tools/go/packages` only. It is its own Go module
(`tools/exportcheck/go.mod`), so the main module's dependency graph does not change and
`go build ./...` / `go vet ./...` at the repository root do not compile it. Build it with
`make check-exports` (or `cd tools/exportcheck && go build -o ../../build/exportcheck .`).

## What it checks
Every exported top-level func/type/alias/const/var, method of an exported type, exported field of
an exported struct (including nested anonymous structs) and method of an exported interface, in
every package whose path contains `internal/`. The module is loaded with `Tests=true` for
{default, `-tags integration`} x {linux, darwin} (CGO off) and merged conservatively: a use in ANY
configuration counts. Each name lands in one category:

| category | meaning |
|---|---|
| needed by production elsewhere | referenced from another package's non-test file |
| only external tests | referenced only from other packages' `_test.go` (relocation candidate: move the test or the code) |
| nobody outside | no other package references it (un-export candidate). Black-box `pkg_test` use of its own package counts as "in-package" |
| exempt | would be flagged but exempted (reason printed) |

## Usage
    make check-exports                  # build + run in the repository's chosen mode
    cd tools/exportcheck && go build -o ../../build/exportcheck . && cd ../..
    build/exportcheck -dir . -exempt tools/exportcheck/exemptions.txt            # text report to stdout
    build/exportcheck -json report.json -text report.txt -v                       # both formats, timings
    build/exportcheck -strict -summary                                            # CI gate (exit 1)
    build/exportcheck -strict-tests                                               # also fail on only-external-tests

Flags: `-dir`, `-pkgs` (default `./...`), `-exempt`, `-json` (`-` = stdout), `-text` (`-` stdout, `""` off),
`-summary`, `-strict`, `-strict-tests`, `-goos linux,darwin`, `-tagsets ,integration` (empty entry = default; join
tags inside a set with `+`), `-v`. Exit codes: 0 ok, 1 strict failure, 2 load/usage error. Output is deterministic.

## Exemptions
`exemptions.txt`: `internal/pkg.Name  # reason`. The reason is mandatory (the tool refuses to run without one).
Keys: `pkg.Func`, `pkg.Type`, `pkg.Type.Method`, `pkg.Type.Field`, `pkg.Iface.Method`. A trailing `.*` matches
by prefix. Stale entries are listed.

Exempted automatically (with reason in the report): method satisfying a stdlib/third-party/other-package
exported interface (checked with `types.Implements`); well-known protocol method names (`Error`, `String`,
`Unwrap`, `Is`, `As`, `Marshal*/Unmarshal*`, `Len/Less/Swap`, `Read/Write/Close`, ...); interface methods
implemented by a type in another package; struct fields with a json/yaml/xml/toml/db/bson... tag; all
exported fields of module structs reachable from arguments of `encoding/*`, `reflect`, `text|html/template`,
yaml, toml calls; embedded fields; names in generated files; `//go:linkname` symbols.

## Fixing a finding
Un-export (`gopls rename -w file:line:col newName`, or `gofmt -r`), or exempt with a reason.
Same-package interface implementations inherit the uses of the interface method they implement.

## Blind spots
Reflection through `any`/`interface{}` values (only statically visible arguments of serialization calls are
followed), `//go:generate` output that is not committed, `go:linkname` pulling from other modules, cgo, plugins,
struct tags consumed by custom reflection, text/template field access by name in template strings,
build constraints outside the 4 configurations (windows, arm-specific), method values stored in `map[string]any`
dispatch tables, and external consumers outside the module (internal/ guarantees there are none).
