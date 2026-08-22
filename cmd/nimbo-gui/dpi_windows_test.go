package main

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WACK (requirement 26, Deck #552) flagged nimbo-gui.exe as not DPI aware:
// neither PerMonitorV2 in a manifest nor any DPI Awareness API call. Windows
// then system-scales the process, so the WebView2 UI renders blurry at any
// display scaling above 100%.
//
// This asserts the real effect on the process, not merely that a function ran:
// after setDPIAwareness, Windows must report this process as per-monitor aware.
func TestSetDPIAwarenessMakesProcessPerMonitorAware(t *testing.T) {
	if err := setDPIAwareness(); err != nil {
		t.Fatalf("setDPIAwareness: %v", err)
	}

	shcore := windows.NewLazySystemDLL("shcore.dll")
	getProcessDpiAwareness := shcore.NewProc("GetProcessDpiAwareness")
	if err := getProcessDpiAwareness.Find(); err != nil {
		t.Skipf("GetProcessDpiAwareness unavailable: %v", err)
	}

	var awareness uint32
	r, _, _ := getProcessDpiAwareness.Call(0, uintptr(unsafe.Pointer(&awareness)))
	if r != 0 {
		t.Fatalf("GetProcessDpiAwareness returned HRESULT 0x%x", r)
	}

	// PROCESS_PER_MONITOR_DPI_AWARE == 2. PerMonitorV2 reports as per-monitor
	// through this legacy API; anything less means Windows is still scaling us.
	const processPerMonitorDPIAware = 2
	if awareness != processPerMonitorDPIAware {
		t.Fatalf("process DPI awareness = %d, want %d (per-monitor)", awareness, processPerMonitorDPIAware)
	}
}
