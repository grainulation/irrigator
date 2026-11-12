#!/bin/sh
# Print the runtime platforms for this change to $GITHUB_OUTPUT (or stdout).
# macOS is skipped only when every changed file is documentation. When the
# changed files cannot be determined, macOS runs.
set -u
case "${EVENT:-}" in
  pull_request) base="${PR_BASE:-}" ;;
  push) base="${PUSH_BEFORE:-}" ;;
  *) base="" ;;
esac

macos=true
if [ -n "$base" ] && [ "$base" != 0000000000000000000000000000000000000000 ] &&
  files=$(git diff --no-renames --name-only "$base" HEAD 2>/dev/null) && [ -n "$files" ]; then
  if ! printf '%s\n' "$files" | grep -Ev '^(docs/.*|.*\.md|LICENSE)$' >/dev/null; then
    macos=false
  fi
fi

if [ "$macos" = true ]; then
  os='["ubuntu-24.04","macos-15"]'
else
  os='["ubuntu-24.04"]'
fi
echo "Platforms: $os"
echo "os=$os" >> "${GITHUB_OUTPUT:-/dev/stdout}"
