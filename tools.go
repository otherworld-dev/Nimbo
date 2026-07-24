//go:build tools

// Package tools anchors build-time dependencies that no ordinary source file
// imports, so `go mod tidy` keeps them in go.mod. golang.org/x/mobile/bind is
// injected by `gomobile bind` when building the Android .aar from ./mobile —
// without this anchor a tidy silently drops the module and breaks that build.
package tools

import _ "golang.org/x/mobile/bind"
