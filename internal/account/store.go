package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store is a small JSON-backed collection of account metadata persisted to a
// single file. Secrets are never written here (see keychain.go).
type Store struct {
	path     string
	Accounts []Account `json:"accounts"`
	// DefaultID selects the active account when several are configured. Empty
	// (or stale) falls back to the first account, which keeps pre-multi-account
	// stores working unchanged.
	DefaultID string `json:"defaultId,omitempty"`
}

// LoadStore reads the account store from path. A missing file yields an empty
// store rather than an error, so first-run is seamless.
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read account store: %w", err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse account store %s: %w", path, err)
	}
	return s, nil
}

// save atomically writes the store to disk (temp file + rename) so a crash
// mid-write cannot corrupt the existing store.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode account store: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write account store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("commit account store: %w", err)
	}
	return nil
}

// Find returns the account with the given ID, or false if absent.
func (s *Store) Find(id string) (Account, bool) {
	for _, a := range s.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// Upsert adds or replaces an account (matched by ID) and persists the store.
func (s *Store) Upsert(a Account) error {
	for i := range s.Accounts {
		if s.Accounts[i].ID == a.ID {
			s.Accounts[i] = a
			return s.save()
		}
	}
	s.Accounts = append(s.Accounts, a)
	return s.save()
}

// Remove deletes the account with the given ID and persists the store. It is a
// no-op (returns nil) if no such account exists. Removing the default account
// clears DefaultID so Default() falls back to the first remaining account.
func (s *Store) Remove(id string) error {
	for i := range s.Accounts {
		if s.Accounts[i].ID == id {
			s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
			if s.DefaultID == id {
				s.DefaultID = ""
			}
			return s.save()
		}
	}
	return nil
}

// Default returns the active account: the one DefaultID points at, falling
// back to the first account when DefaultID is empty or stale. The boolean is
// false when no accounts are configured.
func (s *Store) Default() (Account, bool) {
	if len(s.Accounts) == 0 {
		return Account{}, false
	}
	if s.DefaultID != "" {
		if a, ok := s.Find(s.DefaultID); ok {
			return a, true
		}
	}
	return s.Accounts[0], true
}

// SetDefault makes the account with the given ID the active one and persists
// the store. It fails if no such account exists.
func (s *Store) SetDefault(id string) error {
	if _, ok := s.Find(id); !ok {
		return fmt.Errorf("no account with id %s", id)
	}
	s.DefaultID = id
	return s.save()
}

// ensure the store path's directory exists before first save.
func (s *Store) ensureDir() error {
	return os.MkdirAll(filepath.Dir(s.path), 0o700)
}
