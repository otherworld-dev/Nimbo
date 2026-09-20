//go:build windows

package shellns

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two toggles in quick succession — on, then off before the first task has
// finished — collided: every call wrote the same three files and the same
// task, and the finishing script deleted them from under the next call
// ("schtasks create failed: … cannot find the file specified"), or the next
// call's script replaced the first one before it had started. Exercises the
// real Task Scheduler, so it runs only on request.
func TestRunOutOfContainerBackToBackCallsBothRun(t *testing.T) {
	if os.Getenv("NIMBO_SHELLNS_LIVE") == "" {
		t.Skip("set NIMBO_SHELLNS_LIVE=1 to run against the real Task Scheduler")
	}
	dir := t.TempDir()
	marker := func(n string) string { return filepath.Join(dir, n) }
	body := func(n string) string {
		return "Set-Content -LiteralPath " + psQuote(marker(n)) + " -Value ok\r\n"
	}
	if err := runOutOfContainer("test-a", body("a")); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := runOutOfContainer("test-b", body("b")); err != nil {
		t.Fatalf("second call: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for _, n := range []string{"a", "b"} {
		for {
			if _, err := os.Stat(marker(n)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("script %s never ran", n)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
