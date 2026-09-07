package agent

// Per-pair sync health. The engine's status string is global, so one healthy
// folder's "Up to date" masks another folder that has stopped syncing entirely
// — observed on Android, where a pair whose storage permission was withdrawn
// failed every pass while the UI reported everything was fine.

import (
	"errors"
	"testing"
)

func TestPairHealthsIsEmptyOnAFreshEngine(t *testing.T) {
	e := &Engine{} // zero value: watchers construct one before Run
	if got := e.PairHealths(); len(got) != 0 {
		t.Fatalf("PairHealths() = %v, want empty", got)
	}
}

func TestPairHealthRecordsAFailureAgainstItsFolder(t *testing.T) {
	e := &Engine{}
	p := Pair{LocalDir: "/sd/Photos", RemoteRoot: "Photos"}

	e.notePairResult(p, errors.New("local folder is missing or empty"))

	h, ok := e.PairHealths()["/sd/Photos"]
	if !ok {
		t.Fatal("no health recorded for the failing pair")
	}
	if !h.Failing {
		t.Error("Failing = false, want true")
	}
	if h.LastError == "" {
		t.Error("LastError is empty; the user needs the reason")
	}
	if h.Since.IsZero() {
		t.Error("Since is zero; the UI wants to say how long it has been broken")
	}
}

func TestPairHealthClearsOnASuccessfulPass(t *testing.T) {
	e := &Engine{}
	p := Pair{LocalDir: "/sd/Photos", RemoteRoot: "Photos"}

	e.notePairResult(p, errors.New("boom"))
	e.notePairResult(p, nil)

	if h, ok := e.PairHealths()["/sd/Photos"]; ok {
		t.Fatalf("health still recorded after a success: %+v", h)
	}
}

// One folder failing must not implicate another.
func TestPairHealthIsPerFolder(t *testing.T) {
	e := &Engine{}
	bad := Pair{LocalDir: "/sd/Photos", RemoteRoot: "Photos"}
	good := Pair{LocalDir: "/sd/Docs", RemoteRoot: "Docs"}

	e.notePairResult(bad, errors.New("boom"))
	e.notePairResult(good, nil)

	got := e.PairHealths()
	if _, ok := got["/sd/Photos"]; !ok {
		t.Error("the failing folder lost its health entry")
	}
	if _, ok := got["/sd/Docs"]; ok {
		t.Error("a healthy folder must not appear in PairHealths")
	}
}

// The first failure's timestamp is what "failing since" means; later failures
// of the same run must not keep resetting it.
func TestPairHealthKeepsTheFirstFailureTime(t *testing.T) {
	e := &Engine{}
	p := Pair{LocalDir: "/sd/Photos", RemoteRoot: "Photos"}

	e.notePairResult(p, errors.New("first"))
	first := e.PairHealths()["/sd/Photos"].Since
	e.notePairResult(p, errors.New("second"))
	got := e.PairHealths()["/sd/Photos"]

	if !got.Since.Equal(first) {
		t.Errorf("Since moved from %v to %v on a repeat failure", first, got.Since)
	}
	if got.LastError != "second" {
		t.Errorf("LastError = %q, want the most recent message", got.LastError)
	}
}

// Removing a folder must not leave it listed as broken forever.
func TestForgetPairHealthDropsTheEntry(t *testing.T) {
	e := &Engine{}
	p := Pair{LocalDir: "/sd/Photos", RemoteRoot: "Photos"}
	e.notePairResult(p, errors.New("boom"))

	e.forgetPairHealth(PairKey(p.LocalDir, p.RemoteRoot))

	if len(e.PairHealths()) != 0 {
		t.Fatal("health survived the pair being forgotten")
	}
}
