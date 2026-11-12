GO ?= go
PREFIX ?= $(HOME)/.local

.PHONY: build test check check-fast check-integration check-tools check-exports lint replay-gate acceptance smoke demo durability install

GOLANGCI_LINT_VERSION ?= v2.15.0
LINT_NEW_FROM_REV ?=
LINT_BASE_FLAG = $(if $(LINT_NEW_FROM_REV),--new-from-rev=$(LINT_NEW_FROM_REV))
# EXPORTCHECK_STRICT is the switch for make check-exports and its CI step: 1 (default) = fail on any
# un-export candidate, 0 = report only (exit 0). Load errors always fail.
EXPORTCHECK_STRICT ?= 1

build:
	$(GO) build -trimpath -o irrigator .

test:
	$(GO) test -timeout 30m ./...

# Fast feedback: every internal package's unit tests plus vet. No -run filter, so a
# test is never silently skipped by a stale name list. This intentionally
# complements, never replaces, the full race gate below.
check-fast:
	$(GO) test ./internal/...
	$(GO) vet ./...

# The default (untagged) suite under the race detector, plus vet. Tests behind
# the "integration" build tag are NOT compiled here; see check-integration.
check:
	$(GO) test -race -timeout 30m ./...
	$(GO) vet ./...

# The whole tree with -tags integration (unit tests plus process-supervision,
# controller-drain, re-exec and CLI integration tests) under the race detector.
# No -run filter: a renamed or relocated test must fail loudly, never vanish.
check-integration:
	$(GO) test -race -tags integration -count=1 -timeout 30m ./...
	$(GO) vet -tags integration ./...

# Static analysis: golangci-lint (tuned config in .golangci.yml), formatting and go.mod hygiene.
# Install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
# Set LINT_NEW_FROM_REV=<commit> to report only findings introduced after that commit (baseline).
lint:
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not found; install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)"; exit 1; }
	golangci-lint run $(LINT_BASE_FLAG) ./...
	golangci-lint run $(LINT_BASE_FLAG) --build-tags=integration ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt: files above need formatting"; exit 1; }
	$(GO) mod tidy -diff

# Minimal exported surface for internal/ packages (tools/exportcheck, ~6 s); see EXPORTCHECK_STRICT above.
check-exports:
	cd tools/exportcheck && $(GO) build -o ../../build/exportcheck .
	./build/exportcheck -dir . -exempt tools/exportcheck/exemptions.txt -summary $(if $(filter 1,$(EXPORTCHECK_STRICT)),-strict)

# Tool, packaging and repository-consistency checks (Python standard library only).
check-tools:
	python3 -m unittest discover -s tools -p 'test_*.py'

# Experimental replay gate: redacted failure fixtures (dev and held-out sets) must
# replay through the real executor path to their recorded states. No network, no credentials.
replay-gate:
	sh tools/replay-gate.sh

# Full local acceptance: the default (untagged) race suite and vet, the tagged
# (-tags integration) race suite, tool checks, the exported-surface report, the replay gate, then
# the credential-free first-use workflow, demo and durability check. "make lint" is a separate
# CI gate (it needs golangci-lint installed).
acceptance: check check-integration check-tools check-exports replay-gate smoke demo durability

smoke: build
	sh verification/smoke.sh ./irrigator

demo: build
	sh verification/adaptive-demo.sh ./irrigator

# Stopping a mock swarm keeps its work; resuming repeats none of it.
durability: build
	python3 verification/swarm-durability-test.py

# Installation is explicit; build/demo never modify PATH or install dependencies.
install: build
	install -d "$(DESTDIR)$(PREFIX)/bin"
	install -m 755 irrigator "$(DESTDIR)$(PREFIX)/bin/irrigator"

