#!/usr/bin/env bash
# Checks that the committed Wails bindings and frontend dist match the source.
#
# Regenerates the bindings, rebuilds the frontend, and fails if either changed
# the tree. CI runs it on every push; run it yourself (Git Bash, repo root or
# anywhere) after touching a Go App method, a DTO, or a .svelte file.
#
# Bindings come first: the frontend imports them, so dist is only right once
# they are.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

GEN_DIRS=(cmd/nimbo-gui/frontend/bindings cmd/nimbo-gui/frontend/dist)

want="$(go list -m -f '{{.Version}}' github.com/wailsapp/wails/v3)"
if ! command -v wails3 >/dev/null 2>&1; then
  echo "wails3 not found. Install it with:"
  echo "  go install github.com/wailsapp/wails/v3/cmd/wails3@$want"
  exit 2
fi
have="$(wails3 version 2>&1 | head -n1 | tr -d '[:space:]')"
if [ "$have" != "$want" ]; then
  echo "wails3 is $have but go.mod uses $want; bindings would differ for that"
  echo "reason alone. Install the matching one:"
  echo "  go install github.com/wailsapp/wails/v3/cmd/wails3@$want"
  exit 2
fi

echo "==> Regenerating bindings"
wails3 generate bindings -clean -d cmd/nimbo-gui/frontend/bindings ./cmd/nimbo-gui

echo "==> Building the frontend"
# npm ci in CI for a clean install from the lockfile. Locally npm install:
# npm ci deletes node_modules first, which fails on Windows while an editor's
# language server has one of its native modules loaded.
if [ -n "${CI:-}" ]; then install=ci; else install=install; fi
( cd cmd/nimbo-gui/frontend && npm "$install" && npm run build )

if [ -n "$(git status --porcelain -- "${GEN_DIRS[@]}")" ]; then
  echo
  echo "Generated files are stale. These changed when rebuilt:"
  git status --porcelain -- "${GEN_DIRS[@]}"
  git --no-pager diff --stat -- "${GEN_DIRS[@]}"
  echo
  echo "Run tools/ci/check-generated.sh locally and commit what it changes."
  exit 1
fi
echo "Generated files are fresh."
