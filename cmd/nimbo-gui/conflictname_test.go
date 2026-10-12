package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/transport"
)

// TestConflictNameDoesNotStackMarkers pins the on-demand write-back's conflict
// naming to the same rule as the live engine's: a conflict of an
// already-conflicted copy replaces the old marker instead of appending another.
// Stacking is how a test-VM file grew six markers and crossed MAX_PATH, at
// which point Explorer and Notepad could no longer open it.
func TestConflictNameDoesNotStackMarkers(t *testing.T) {
	got := conflictName("Notes/New Text Document (conflicted copy 2026-08-16 000722).txt")
	if strings.Count(got, "(conflicted copy") != 1 {
		t.Fatalf("marker stacked: %q", got)
	}
	if !strings.HasPrefix(got, "Notes/New Text Document (conflicted copy ") || !strings.HasSuffix(got, ").txt") {
		t.Fatalf("unexpected shape: %q", got)
	}

	// Six stacked markers (the real VM filename) collapse back to one.
	stacked := "New Text Document" + strings.Repeat(" (conflicted copy 2026-08-16 000722)", 6) + ".txt"
	if got := conflictName(stacked); strings.Count(got, "(conflicted copy") != 1 {
		t.Fatalf("stacked markers survived: %q", got)
	}

	// A dotfile keeps its whole name (no extension split) and still gets
	// exactly one marker.
	got = conflictName(".htaccess (conflicted copy 2025-01-02 030405)")
	if !strings.HasPrefix(got, ".htaccess (conflicted copy ") || strings.Count(got, "(conflicted copy") != 1 {
		t.Fatalf("dotfile: got %q", got)
	}
}

// Parking the server's copy must never replace an earlier parked copy: when
// the name is taken on the server the next one is tried (Deck #924). A park
// that fails for another reason is reported as it is, with nothing retried.
func TestParkNameMovesOnWhenTheNameIsTaken(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	taken := map[string]bool{
		"Notes/doc (conflicted copy 2026-10-12 090000).txt": true,
		"Notes/doc (conflicted copy 2026-10-12 090001).txt": true,
	}
	var tried []string
	got, err := parkName("Notes/doc.txt", now, func(dst string) error {
		tried = append(tried, dst)
		if taken[dst] {
			return fmt.Errorf("MOVE: %w", transport.ErrDestinationExists)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "Notes/doc (conflicted copy 2026-10-12 090002).txt"; got != want {
		t.Fatalf("parked as %q, want %q (tried %v)", got, want, tried)
	}

	boom := errors.New("server said no")
	calls := 0
	if _, err := parkName("Notes/doc.txt", now, func(string) error { calls++; return boom }); !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err = %v after %d calls, want the server's error after one", err, calls)
	}

	// A conflicted copy parked this very second: its new name is its own, so
	// that one is skipped without asking the server to move a file onto itself.
	self := "Notes/doc (conflicted copy 2026-10-12 090000).txt"
	tried = nil
	got, err = parkName(self, now, func(dst string) error { tried = append(tried, dst); return nil })
	if err != nil || got == self || len(tried) != 1 {
		t.Fatalf("parked %q as %q (tried %v, err %v)", self, got, tried, err)
	}
}

// Two parks must never be in flight together. On-demand uploads run in
// parallel, and two files that map to the same conflicted-copy name sent their
// MOVEs to the server in the same instant: Nextcloud's file locking then
// refused BOTH with a 500 (LockedException on the destination), and both
// retried together for ever (seen on the test VM, Deck #924). One at a time,
// the second finds the name taken and takes the next one.
func TestParkNameRunsOneAtATime(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	taken := map[string]bool{}
	move := func(dst string) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		busy := inFlight > 1
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // the request is on the wire
		mu.Lock()
		defer mu.Unlock()
		inFlight--
		if busy {
			return errors.New("500: LockedException") // what the server did to both
		}
		if taken[dst] {
			return transport.ErrDestinationExists
		}
		taken[dst] = true
		return nil
	}
	srcs := []string{"Notes/doc.txt", "Notes/doc (conflicted copy 2026-08-16 000722).txt", "Notes/doc (conflicted copy 2026-08-17 000722).txt"}
	got := make([]string, len(srcs))
	errs := make([]error, len(srcs))
	var wg sync.WaitGroup
	for i, src := range srcs {
		wg.Add(1)
		go func(i int, src string) {
			defer wg.Done()
			got[i], errs[i] = parkName(src, now, move)
		}(i, src)
	}
	wg.Wait()
	if maxInFlight != 1 {
		t.Errorf("%d parks were in flight at once, want 1", maxInFlight)
	}
	seen := map[string]bool{}
	for i := range srcs {
		if errs[i] != nil {
			t.Fatalf("%s: %v", srcs[i], errs[i])
		}
		if seen[got[i]] {
			t.Fatalf("the name %q was given to two files", got[i])
		}
		seen[got[i]] = true
	}
}
