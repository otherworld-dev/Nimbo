#!/usr/bin/env bash
# Builds Nimbo for Linux: the CLI (pure Go) and the Wails v3 GUI (needs the
# native GTK3 + WebKit2GTK deps — see README.md). Run on a Linux machine from
# anywhere; paths are resolved relative to this script.
#
#   packaging/linux/build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
mkdir -p bin

echo "==> CLI (pure Go, no cgo)"
CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/nimbo ./cmd/nimbo

echo "==> Frontend (Vite build, embedded into the GUI)"
( cd cmd/nimbo-gui/frontend && npm ci && npm run build )

echo "==> GUI (Wails v3 — GTK3 + WebKit2GTK, cgo)"
# No -H windowsgui here (Windows-only); on Linux a plain build is a GUI app.
CGO_ENABLED=1 go build -ldflags "-s -w" -o bin/nimbo-gui ./cmd/nimbo-gui

echo
echo "Built:"
echo "  bin/nimbo      (CLI)"
echo "  bin/nimbo-gui  (GUI)"
echo "Run the GUI: ./bin/nimbo-gui"
