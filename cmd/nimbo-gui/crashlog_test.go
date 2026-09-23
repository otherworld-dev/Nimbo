package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each start leaves one header line, so a crash report that follows can be
// told apart by build and run, and the file is cut back once it grows large.
func TestOpenCrashLogHeadersAndTrims(t *testing.T) {
	p := filepath.Join(t.TempDir(), "crash.log")
	old := strings.Repeat("x", crashLogMax) + "\npanic: from an old run\n"
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openCrashLog(p, "0.1.8.308")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	b, _ := os.ReadFile(p)
	s := string(b)
	if len(s) > crashLogKeep+200 {
		t.Errorf("crash.log is %d bytes after trimming, want about %d", len(s), crashLogKeep)
	}
	if !strings.Contains(s, "panic: from an old run") {
		t.Error("trimming dropped the most recent report")
	}
	last := s[strings.LastIndex(strings.TrimRight(s, "\n"), "\n")+1:]
	if !strings.HasPrefix(last, "--- nimbo-gui 0.1.8.308 started ") {
		t.Errorf("last line = %q, want this run's header", last)
	}
}

// The log viewer shows crash.log only when it holds a report, not just the
// start headers every run writes.
func TestCrashReportTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "crash.log")
	if got := crashReportTail(p); got != "" {
		t.Errorf("missing file: %q", got)
	}
	os.WriteFile(p, []byte("--- nimbo-gui 1 started x pid 1\n--- nimbo-gui 1 started y pid 2\n"), 0o644)
	if got := crashReportTail(p); got != "" {
		t.Errorf("headers only: %q", got)
	}
	os.WriteFile(p, []byte("--- nimbo-gui 1 started x pid 1\npanic: boom\n\ngoroutine 1 [running]:\n"), 0o644)
	if got := crashReportTail(p); !strings.Contains(got, "panic: boom") {
		t.Errorf("report not shown: %q", got)
	}
}
