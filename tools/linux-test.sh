#!/usr/bin/env bash
# Run the Go test suite on Linux inside a container. Opt-in; not part of make check.
#
#   tools/linux-test.sh [--no-race] [GO_TEST_ARGS...]
#
# With no GO_TEST_ARGS it runs `go vet ./...` and `go test -race -count=1 ./...`,
# the same two with `-tags integration` (integration tests are not compiled
# otherwise), then the tmux executor tests again verbosely against a real tmux
# installed in the image. GO_TEST_ARGS replace the default package list and filters, for
# example: tools/linux-test.sh ./... -run Cleanup
#
# Runtime: the first of Apple `container` (macOS), podman, docker. Override with
# LINUX_TEST_RUNTIME. The repository is mounted read-only at /src; the module
# and build caches live in the container's own writable layer (/cache), which
# --rm discards afterwards (a host bind mount cannot be chowned for the
# unprivileged user under Apple `container`). The suite runs as an unprivileged user, as CI does. Network is
# needed to pull the image and download modules.
set -u

root=$(cd "$(dirname "$0")/.." && pwd)
race=-race
if [ "${1:-}" = "--no-race" ]; then
  race=""
  shift
fi

rt=${LINUX_TEST_RUNTIME:-}
if [ -z "$rt" ]; then
  for c in container podman docker; do
    if [ "$c" = container ] && [ "$(uname -s)" != Darwin ]; then continue; fi
    if command -v "$c" >/dev/null 2>&1; then
      rt=$c
      break
    fi
  done
fi
if [ -z "$rt" ]; then
  echo "SKIP  no container, podman or docker on PATH" >&2
  exit 0
fi

# Match go.mod: prefer the exact toolchain image, then its minor, then the
# `go` line's minor. GOTOOLCHAIN=auto downloads the exact toolchain if the
# image's Go is older.
toolchain=$(sed -n 's/^toolchain go//p' "$root/go.mod" | head -n 1)
golang=$(sed -n 's/^go \([0-9]*\.[0-9]*\).*/\1/p' "$root/go.mod" | head -n 1)
candidates=()
if [ -n "$toolchain" ]; then
  candidates+=("golang:$toolchain" "golang:${toolchain%.*}")
fi
candidates+=("golang:$golang")

pull() {
  case "$rt" in
  container) container image pull "$1" ;;
  *) "$rt" pull "$1" ;;
  esac
}
image=""
for c in "${candidates[@]}"; do
  if pull "$c" >/dev/null 2>&1; then
    image=$c
    break
  fi
  echo "image $c unavailable" >&2
done
if [ -z "$image" ]; then
  echo "FAIL  no golang image could be pulled (tried: ${candidates[*]})" >&2
  exit 1
fi
echo "runtime: $rt  image: $image  go.mod toolchain: ${toolchain:-none}"

if [ "$#" -eq 0 ]; then
  set -- ./...
  default=1
else
  default=0
fi

inner='set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq tmux >/dev/null
echo "tmux: $(tmux -V)  go: $(go version)"
useradd -m tester
export GOTOOLCHAIN=auto GOCACHE=/cache/go-build GOMODCACHE=/cache/mod GOFLAGS=-buildvcs=false
mkdir -p /cache/go-build /cache/mod /cache/tmp
chown -R tester /cache
run() { runuser -u tester -- env PATH="$PATH" HOME=/home/tester TMPDIR=/cache/tmp GOTOOLCHAIN=auto GOCACHE=/cache/go-build GOMODCACHE=/cache/mod GOFLAGS=-buildvcs=false "$@"; }
cd /src
status=0
if [ "$DEFAULT" = 1 ]; then
  run go vet ./... || status=1
  run go vet -tags integration ./... || status=1
fi
run go test $RACE -count=1 "$@" || status=1
if [ "$DEFAULT" = 1 ]; then
  echo "--- integration-tagged suite"
  run go test $RACE -count=1 -tags integration ./... || status=1
  echo "--- tmux executor tests with real tmux in the image"
  run go test $RACE -count=1 -v -tags integration ./... -run "Tmux" || status=1
fi
exit $status'

"$rt" run --rm --cpus 4 --memory 6g \
  -v "$root":/src:ro \
  -e "DEFAULT=$default" -e "RACE=$race" \
  "$image" bash -c "$inner" linux-test "$@"
code=$?
if [ "$code" -eq 0 ]; then echo "PASS  linux tests ($image)"; else echo "FAIL  linux tests ($image), exit $code"; fi
exit "$code"
