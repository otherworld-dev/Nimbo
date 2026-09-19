package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// An upload put off because another program still has the file open is not
// "Up to date": the user would close the laptop thinking it had gone.
func TestWaitingStatusNamesAFileOpenInAnotherProgram(t *testing.T) {
	for _, tc := range []struct {
		held, busy []string
		want       string
	}{
		{nil, nil, "Up to date"},
		{nil, []string{"To Sort/archive.pst"}, "Waiting — archive.pst is open in another program"},
		{nil, []string{"a.pst", "b.pst"}, "Waiting — 2 files are open in other programs"},
		{[]string{"Budget.xlsx"}, nil, "Waiting — Budget.xlsx is in use by someone else"},
		{[]string{"Budget.xlsx"}, []string{"a.pst"}, "Waiting — 2 files are in use"},
	} {
		if got := waitingStatus(tc.held, tc.busy); got != tc.want {
			t.Errorf("waitingStatus(%v, %v) = %q, want %q", tc.held, tc.busy, got, tc.want)
		}
	}
}

// Closing a file need not write to it, so need not raise a watcher event, and
// the put-off upload would wait for the hourly pass. Once the program lets go,
// the path is nudged into a quick sync, once.
func TestAPutOffUploadIsNudgedOnceTheProgramLetsGo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := Pair{LocalDir: t.TempDir()}
	nudge := make(chan string, 4)
	e := &Engine{runCtx: ctx, nudges: map[string]chan string{PairKey(p.LocalDir, p.RemoteRoot): nudge}}

	var gone atomic.Bool
	ow, oe := writerGone, closedCheckEvery
	writerGone = func(string) bool { return gone.Load() }
	closedCheckEvery = 10 * time.Millisecond
	t.Cleanup(func() { writerGone, closedCheckEvery = ow, oe })

	const abs = `C:\Sync\archive.pst`
	e.awaitClosed(p, abs)
	e.awaitClosed(p, abs) // the next pass puts it off again: still one watch
	select {
	case got := <-nudge:
		t.Fatalf("nudged %q while the program still had it open", got)
	case <-time.After(80 * time.Millisecond):
	}
	gone.Store(true)
	select {
	case got := <-nudge:
		if got != abs {
			t.Fatalf("nudged %q, want %q", got, abs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never nudged after the program let go")
	}
	select {
	case got := <-nudge:
		t.Fatalf("nudged twice (%q)", got)
	case <-time.After(80 * time.Millisecond):
	}
}
