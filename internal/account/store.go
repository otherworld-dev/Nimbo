package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/otherworld/nimbo/internal/atomicfile"
)

// Store is a small JSON-backed collection of account metadata persisted to a
// single file. Secrets are never written here (see keychain.go).
//
// Nothing here writes the file on its own. Upsert, Remove and SetDefault edit
// the copy in memory, and Update is the one way to persist a change: it loads
// the file, applies the edit and saves, all under the file's lock. That is
// deliberate. When each method saved as it went, every caller loaded its own
// copy and wrote the whole file back, so two callers overlapping both started
// from the same copy and the one that saved second threw the other's change
// away (Deck #693).
type Store struct {
	path     string
	Accounts []Account `json:"accounts"`
	// DefaultID selects the active account when several are configured. Empty
	// (or stale) falls back to the first account, which keeps pre-multi-account
	// stores working unchanged.
	DefaultID string `json:"defaultId,omitempty"`
}

// LoadStore reads the account store from path. A missing file yields an empty
// store rather than an error, so first-run is seamless. It is a read: an edit
// made to the result goes nowhere unless it is made inside Update.
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

// Update loads the store at path, hands it to mutate and saves the result,
// holding the file's lock across all three so that overlapping updates from
// this process apply one after the other instead of the later one overwriting
// the earlier one's change. It is the only way to persist a change.
//
// An error from mutate abandons the update and is returned; the file is left
// as it was. An update that changes nothing writes nothing, so a store that
// does not exist yet is not created by it.
//
// The lock is this process's only: the GUI and a concurrent CLI run still race
// each other, and one of them wins whole. Closing that needs a lock file on
// disk, and internal/config carries the same limit.
//
// mutate runs under the lock, so it must not call Update itself.
func Update(path string, mutate func(*Store) error) error {
	mu := atomicfile.Mutex(path)
	mu.Lock()
	defer mu.Unlock()

	s, err := LoadStore(path)
	if err != nil {
		return err
	}
	before, err := s.encode()
	if err != nil {
		return err
	}
	if err := mutate(s); err != nil {
		return err
	}
	after, err := s.encode()
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		return nil
	}
	if err := atomicfile.WriteLocked(s.path, after, 0o600); err != nil {
		return fmt.Errorf("save account store: %w", err)
	}
	return nil
}

func (s *Store) encode() ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode account store: %w", err)
	}
	return data, nil
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

// Upsert adds or replaces an account (matched by ID) in memory.
func (s *Store) Upsert(a Account) {
	for i := range s.Accounts {
		if s.Accounts[i].ID == a.ID {
			s.Accounts[i] = a
			return
		}
	}
	s.Accounts = append(s.Accounts, a)
}

// Remove deletes the account with the given ID in memory. It is a no-op if no
// such account exists. Removing the default account clears DefaultID so
// Default() falls back to the first remaining account.
func (s *Store) Remove(id string) {
	for i := range s.Accounts {
		if s.Accounts[i].ID == id {
			s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
			if s.DefaultID == id {
				s.DefaultID = ""
			}
			return
		}
	}
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

// SetDefault makes the account with the given ID the active one, in memory.
// It fails if no such account exists.
func (s *Store) SetDefault(id string) error {
	if _, ok := s.Find(id); !ok {
		return fmt.Errorf("no account with id %s", id)
	}
	s.DefaultID = id
	return nil
}
