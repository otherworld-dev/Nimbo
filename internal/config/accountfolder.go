package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// OnDemandRoot is the folder this account last mounted as its virtual-files
	// root. A background account reconnects to exactly this folder; before it
	// was recorded, a registration lost to an unmount made it pick a new folder.
	OnDemandRoot string `json:"onDemandRoot,omitempty"`
	// RootMissingSince is when OnDemandRoot was first found missing while
	// still registered (RFC 3339), most likely renamed or moved; "" otherwise.
	RootMissingSince string `json:"rootMissingSince,omitempty"`
	// RootVolume identifies the disk OnDemandRoot was mounted on (its volume
	// serial), so a different disk given the same drive letter counts as the
	// drive being away rather than the folder being gone.
	RootVolume string `json:"rootVolume,omitempty"`
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
// inherit it. Call it for the DEFAULT account only, with the IDs of the other
// accounts: the global value was written by whichever account last ran setup,
// usually but not always the default one. A folder that holds another
// account's sync folders and none of this account's belongs to that other
// account, so it is not claimed here (this account then picks a folder later).
// An account that already has its own folder keeps it. A no-op when unscoped.
func (d Dirs) MigrateAccountFolder(others []string) {
	if d.acct == "" {
		return
	}
	_ = d.UpdateSettings(func(g *Settings) {
		if g.BaseDir == "" && len(g.RememberedPairs) == 0 {
			return
		}
		base := g.BaseDir
		if base != "" && !d.ownsFolder(base) && anyOverlaps(base, others, d) {
			base = ""
		}
		err := d.UpdateAccountState(func(s *AccountState) {
			if s.BaseDir == "" {
				s.BaseDir = base
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

// ownsFolder reports whether any of this account's sync folders (live or,
// from the legacy global list, parked) is dir or overlaps it.
func (d Dirs) ownsFolder(dir string) bool {
	for _, p := range d.pairDirs(true) {
		if overlaps(dir, p) {
			return true
		}
	}
	return false
}

// anyOverlaps reports whether dir overlaps a sync folder of one of the given
// accounts (their live and parked pairs).
func anyOverlaps(dir string, accounts []string, d Dirs) bool {
	for _, id := range accounts {
		for _, p := range d.WithAccount(id).pairDirs(false) {
			if overlaps(dir, p) {
				return true
			}
		}
	}
	return false
}

// pairDirs lists this account's live and parked pair folders; withLegacy also
// counts the legacy global parked list, which the default account inherits.
func (d Dirs) pairDirs(withLegacy bool) []string {
	var out []string
	if pairs, err := d.LoadPairs(); err == nil {
		for _, p := range pairs {
			out = append(out, p.LocalDir)
		}
	}
	if s, err := d.LoadAccountState(); err == nil {
		for _, p := range s.RememberedPairs {
			out = append(out, p.LocalDir)
		}
	}
	if withLegacy {
		if g, err := d.LoadSettings(); err == nil {
			for _, p := range g.RememberedPairs {
				out = append(out, p.LocalDir)
			}
		}
	}
	return out
}

// overlaps reports whether a and b are the same folder or one is inside the
// other (case-insensitive on Windows, as filepath.Rel is).
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }

func within(p, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
