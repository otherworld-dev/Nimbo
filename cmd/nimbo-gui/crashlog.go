package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

// The GUI build has no console, so the report Go prints when the process dies
// of an unrecovered panic or a fatal runtime error went nowhere: the log just
// stopped. That is all there was of 0.1.8.307 exiting a second after its
// post-update relaunch (Deck #714). crash.log sits beside nimbo.log, so the
// problem report's zip of the logs folder carries it.
const (
	crashLogMax  = 256 << 10 // cut the file back at startup once it passes this
	crashLogKeep = 64 << 10  // how much of the end is kept when it is cut
)

// setupCrashLog sends crash reports to path as well as stderr. Best effort: a
// failure costs only the report.
func setupCrashLog(path string) {
	f, err := openCrashLog(path, version)
	if err != nil {
		return
	}
	// SetCrashOutput duplicates the handle; ours is not needed after it.
	defer f.Close()
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
}

// openCrashLog opens path to append, first cutting it back to its last
// crashLogKeep bytes if it has grown past crashLogMax, and writes this run's
// header line so a report that follows says which build and run it was.
func openCrashLog(path, ver string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > crashLogMax {
		if b, rerr := os.ReadFile(path); rerr == nil {
			tail := b[len(b)-crashLogKeep:]
			if i := strings.IndexByte(string(tail), '\n'); i >= 0 {
				tail = tail[i+1:]
			}
			_ = os.WriteFile(path, tail, 0o644)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "--- nimbo-gui %s started %s pid %d\n", ver, time.Now().Format(time.RFC3339), os.Getpid())
	return f, nil
}

// crashReportTail returns the end of crash.log when it holds a crash report
// (anything besides the per-run header lines), for the log viewer; "" if not.
func crashReportTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const max = 32 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		_, _ = f.Seek(-max, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	s := string(b)
	for _, line := range strings.Split(s, "\n") {
		if line != "" && !strings.HasPrefix(line, "--- nimbo-gui ") {
			return s
		}
	}
	return ""
}
