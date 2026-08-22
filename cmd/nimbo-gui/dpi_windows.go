package main

import (
	"golang.org/x/sys/windows"
)

// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2. The context values are sentinel
// pseudo-handles, not real pointers: (HANDLE)-4 for PerMonitorV2.
const dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(0) - 3 // -4 as uintptr

var (
	user32dpi                         = windows.NewLazySystemDLL("user32.dll")
	procSetProcessDpiAwarenessContext = user32dpi.NewProc("SetProcessDpiAwarenessContext")
)

// setDPIAwareness declares the process per-monitor DPI aware (V2).
//
// Without it Windows system-scales us and the WebView2 UI renders blurry at any
// display scaling above 100% — WACK requirement 26 flags exactly this (Deck
// #552). PerMonitorV2 additionally gives child HWNDs automatic DPI change
// notifications, which is what the flyout's WindowDPIChanged handler wants when
// a window is dragged between monitors with different scaling.
//
// This MUST run before any window exists — Windows latches the process default
// at first use — so main calls it as its first statement. The alternative, an
// embedded application manifest, applies even earlier, but needs a committed
// .syso and would risk clashing with any manifest Wails embeds; the documented
// API is enough here and keeps `go build` dependency-free.
//
// A failure is not fatal: an old Windows without the API, or a process whose
// awareness is already fixed, just means we render as before.
func setDPIAwareness() error {
	if err := procSetProcessDpiAwarenessContext.Find(); err != nil {
		return err
	}
	r, _, err := procSetProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAwareV2)
	if r == 0 {
		return err
	}
	return nil
}
