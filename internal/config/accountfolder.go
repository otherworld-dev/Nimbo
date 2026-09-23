package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AccountState is the folder setup that belongs to ONE account: where its
// files live, and the live sync pairs parked while on-demand mode is on.
//
// Both used to sit in the global settings file, so every account shared them.
// That is how a second account was offered, and in on-demand mode mounted, the
// first account's folder (GitHub #11): the active account always read the one
// global folder, and each account switch moved folders around and left the
// previous registration behind.
type AccountState struct {
	// BaseDir is the account's local folder: the whole-account root, and the
	// parent that newly chosen folders are placed under.
	BaseDir string `json:"baseDir,omitempty"`
	// RememberedPairs are the live sync pairs cleared when entering on-demand
	// mode, restored when the user switches back to live.
	RememberedPairs []SyncPair `json:"rememberedPairs,omitempty"`
}

// AccountStateFile is the path to the account's folder setup. Scoped like
// PairsFile; unscoped it names a legacy file nothing writes.
func (d Dirs) AccountStateFile() string {
	if d.acct != "" {
		return filepath.Join(d.Config, "account-"+d.acct+".json")
	}
	return filepath.Join(d.Config, "account.json")
}

// LoadAccountState reads the account's folder setup; a missing file yields the
// zero value.
func (d Dirs) LoadAccountState() (AccountState, error) {
	var s AccountState
	data, err := os.ReadFile(d.AccountStateFile())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read account state: %w", err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse account state %s: %w", d.AccountStateFile(), err)
	}
	return s, nil
}

// UpdateAccountState applies mutate to the account's folder setup and persists
// it, holding the file's lock across the read-modify-write (see UpdateSettings
// for why a bare load-then-save loses updates).
func (d Dirs) UpdateAccountState(mutate func(*AccountState)) error {
	mu := fileMutex(d.AccountStateFile())
	mu.Lock()
	defer mu.Unlock()

	s, err := d.LoadAccountState()
	if err != nil {
		return err
	}
	mutate(&s)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileLocked(d.AccountStateFile(), data, 0o600)
}

// MigrateAccountFolder moves the folder setup out of the global settings file
// into this account's own, then clears the global copy so no other account can
// inherit it. Call it for the DEFAULT account only: the global value was always
// written by whichever account was active, so on upgrade it belongs to that one.
// An account that already has its own folder keeps it. A no-op when unscoped.
func (d Dirs) MigrateAccountFolder() {
	if d.acct == "" {
		return
	}
	_ = d.UpdateSettings(func(g *Settings) {
		if g.BaseDir == "" && len(g.RememberedPairs) == 0 {
			return
		}
		err := d.UpdateAccountState(func(s *AccountState) {
			if s.BaseDir == "" {
				s.BaseDir = g.BaseDir
			}
			if len(s.RememberedPairs) == 0 {
				s.RememberedPairs = g.RememberedPairs
			}
		})
		if err != nil {
			return // keep the global copy; the next start retries
		}
		g.BaseDir = ""
		g.RememberedPairs = nil
	})
}
