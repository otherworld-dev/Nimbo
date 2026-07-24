package config

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pkgidKernel32                        = windows.NewLazySystemDLL("kernel32.dll")
	procPkgidGetCurrentPackageFamilyName = pkgidKernel32.NewProc("GetCurrentPackageFamilyName")
)

// hasPackageIdentity reports whether this process runs with MSIX package
// identity (the installed, packaged app) as opposed to a bare dev build or
// the CLI run from a shell. A var so tests can pin either mode.
var hasPackageIdentity = func() bool {
	var length uint32
	procPkgidGetCurrentPackageFamilyName.Call(uintptr(unsafe.Pointer(&length)), 0)
	return length != 0
}
