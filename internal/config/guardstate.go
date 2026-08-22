package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// GuardState is one folder's damage-guard state. An entry exists only while
// there is something to record — a freeze, or the one-pass exemption granted
// when the user resumes. Absence is the normal, healthy case.
type GuardState struct {
	// Frozen is set while the guard has paused this folder pending review.
	Frozen *Freeze `json:"frozen,omitempty"`
	// ExemptNext skips the guard for exactly one pass, granted when the user
	// reviews a freeze and chooses to resume.
	ExemptNext bool `json:"exemptNext,omitempty"`
}

// Freeze records a tripped guard so the user can review it and so the freeze
// survives a restart. Kept here rather than in the state DB so a state reset
// cannot silently unfreeze a folder.
type Freeze struct {
	AtUnix     int64    `json:"atUnix"`
	Deletions  int      `json:"deletions"`
	Overwrites int      `json:"overwrites"`
	Known      int      `json:"known"`
	Reason     string   `json:"reason"`
	Sample     []string `json:"sample,omitempty"` // a few affected paths, for the review UI
}

// GuardStates maps PairKey to a folder's guard state.
type GuardStates map[string]GuardState

// GuardStateFile is the path to this account's guard state.
func (d Dirs) GuardStateFile() string {
	if d.acct != "" {
		return filepath.Join(d.Config, "guard-"+d.acct+".json")
	}
	return filepath.Join(d.Config, "guard.json")
}

// LoadGuardState reads this account's guard state.
//
// A MISSING file is the normal case — nothing frozen — and yields an empty set.
// A file that exists but does not PARSE is an error, and callers must treat it
// as such: it records which folders the guard paused, so reading it as "nothing
// frozen" would resume a paused folder and apply the changes the guard stopped.
func (d Dirs) LoadGuardState() (GuardStates, error) {
	data, err := os.ReadFile(d.GuardStateFile())
	if errors.Is(err, os.ErrNotExist) {
		return GuardStates{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read guard state: %w", err)
	}
	var set GuardStates
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("parse guard state %s: %w", d.GuardStateFile(), err)
	}
	if set == nil {
		set = GuardStates{}
	}
	return set, nil
}

// UpdateGuardState applies mutate to the current set and persists the result,
// holding the file's lock across the whole read-modify-write.
//
// PREFER THIS over Load + Save. The file is written from the GUI's handler
// goroutines and from the engine (recording a freeze, spending an exemption); a
// bare load-mutate-save loses whichever update lands second.
//
// mutate runs under the lock, so it must not call back into UpdateGuardState.
func (d Dirs) UpdateGuardState(mutate func(GuardStates)) error {
	mu := fileMutex(d.GuardStateFile())
	mu.Lock()
	defer mu.Unlock()

	set, err := d.LoadGuardState()
	if err != nil {
		return err
	}
	mutate(set)
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	return writeFileLocked(d.GuardStateFile(), data, 0o600)
}
