package cli

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// normalizeServer ensures a server URL has a scheme, defaulting to https, and
// trims any trailing slash.
func normalizeServer(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return strings.TrimRight(s, "/")
}

// openBrowser attempts to open url in the user's default browser. A non-nil
// error simply means the caller should fall back to showing the URL.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// humanSize formats a byte count in IEC units (KiB, MiB, …).
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
