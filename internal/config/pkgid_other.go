//go:build !windows

package config

// hasPackageIdentity is only meaningful on Windows (MSIX). Elsewhere every
// install is "the real app", so the dev-dir split never applies.
var hasPackageIdentity = func() bool { return true }
