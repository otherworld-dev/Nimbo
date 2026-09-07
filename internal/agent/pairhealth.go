package agent

// Per-pair sync health.
//
// The engine publishes ONE status string for the whole account, which is fine
// until folders disagree: a healthy folder's "Up to date" overwrites a folder
// that has stopped syncing, and the UI then tells the user everything is fine
// while nothing is reaching the server. Observed on Android, where a pair on
// shared storage failed every pass after its permission was withdrawn.
//
// This records the last outcome per pair so a UI can be honest per folder. It
// is deliberately separate from the damage guard's freeze: a freeze is a
// deliberate pause awaiting review, this is "the last pass errored".

import (
	"sync"
	"time"
)

// PairHealth is the last failure recorded for one sync folder. A healthy folder
// has no entry at all.
type PairHealth struct {
	Failing bool
	// LastError is the most recent message — the reason worth showing.
	LastError string
	// Since is when the folder STARTED failing, not the latest attempt, so a UI
	// can say how long it has been broken.
	Since time.Time
}

// pairHealthState is lazily created so a zero-value Engine (constructed before
// Run, and by tests) works without ceremony.
type pairHealthState struct {
	mu sync.Mutex
	m  map[string]pairHealthEntry
}

type pairHealthEntry struct {
	localDir  string
	lastError string
	since     time.Time
}

// notePairResult records the outcome of one pair's sync pass: an error starts
// (or updates) a failing streak, a success clears it.
func (e *Engine) notePairResult(p Pair, err error) {
	key := PairKey(p.LocalDir, p.RemoteRoot)
	s := e.pairHealth()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.m, key)
		return
	}
	entry, existing := s.m[key]
	if !existing {
		entry.since = time.Now()
	}
	entry.localDir = p.LocalDir
	entry.lastError = err.Error()
	s.m[key] = entry
}

// forgetPairHealth drops a pair's record — for when the pair itself goes, so a
// removed folder cannot linger in the UI as permanently broken.
func (e *Engine) forgetPairHealth(key string) {
	s := e.pairHealth()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

// PairHealths returns the folders whose last sync pass failed, keyed by local
// directory (matching FrozenViews, which UIs join against the pair list).
// Healthy folders are absent rather than present-and-false.
func (e *Engine) PairHealths() map[string]PairHealth {
	s := e.pairHealth()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]PairHealth, len(s.m))
	for _, entry := range s.m {
		out[entry.localDir] = PairHealth{
			Failing:   true,
			LastError: entry.lastError,
			Since:     entry.since,
		}
	}
	return out
}

func (e *Engine) pairHealth() *pairHealthState {
	e.pairHealthOnce.Do(func() {
		e.pairHealthState = &pairHealthState{m: map[string]pairHealthEntry{}}
	})
	return e.pairHealthState
}
