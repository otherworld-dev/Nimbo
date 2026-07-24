package account

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/zalando/go-keyring"
)

// keychainService is the service name under which app passwords are stored in
// the OS keychain (Windows Credential Manager, macOS Keychain, Secret Service).
const keychainService = "nimbo"

// legacyKeychainService is the pre-rename service name; LoadSecret migrates
// secrets found here to keychainService so existing installs keep working.
const legacyKeychainService = "nextclient"

// ErrNoSecret is returned when no app password is stored for an account.
var ErrNoSecret = errors.New("no app password stored for account")

// SecretStore abstracts where app passwords live. Desktop builds use the OS
// keychain (the default below); platforms without one — Android, where the app
// supplies a Keystore-backed store — replace it via SetSecretStore.
type SecretStore interface {
	// Get returns the stored app password, or ErrNoSecret when none exists.
	Get(accountID string) (string, error)
	Set(accountID, appPassword string) error
	// Delete is a no-op (nil) when no secret is stored.
	Delete(accountID string) error
}

// secrets is the active backend, atomic so installing a replacement (the
// mobile facade's Keystore adapter) can never tear against concurrent reads
// from engine goroutines — e.g. when Android recreates its service while a
// previous engine is still draining.
var secrets atomic.Pointer[SecretStore]

func init() {
	var s SecretStore = keychainStore{}
	secrets.Store(&s)
}

// SetSecretStore replaces the OS-keychain backend. Install it at process
// startup, before any account operation (the mobile facade does this in
// NewClient). s must not be nil.
func SetSecretStore(s SecretStore) {
	if s == nil {
		panic("account: SetSecretStore(nil)")
	}
	secrets.Store(&s)
}

// SaveSecret stores an app password for the account.
func SaveSecret(accountID, appPassword string) error {
	return (*secrets.Load()).Set(accountID, appPassword)
}

// LoadSecret retrieves an account's app password.
func LoadSecret(accountID string) (string, error) {
	return (*secrets.Load()).Get(accountID)
}

// DeleteSecret removes an account's app password. It is a no-op (returns nil)
// if no secret was present.
func DeleteSecret(accountID string) error {
	return (*secrets.Load()).Delete(accountID)
}

// keychainStore is the desktop backend: the OS keychain.
type keychainStore struct{}

func (keychainStore) Set(accountID, appPassword string) error {
	if err := keyring.Set(keychainService, accountID, appPassword); err != nil {
		return fmt.Errorf("store app password in keychain: %w", err)
	}
	return nil
}

// Get retrieves an app password from the OS keychain. If it's not under the
// current service but exists under the legacy (pre-rename) one, it is migrated
// across transparently.
func (keychainStore) Get(accountID string) (string, error) {
	pw, err := keyring.Get(keychainService, accountID)
	if err == nil {
		return pw, nil
	}
	if !errors.Is(err, keyring.ErrNotFound) {
		return "", fmt.Errorf("read app password from keychain: %w", err)
	}
	// Fall back to the legacy service and migrate.
	if legacy, lerr := keyring.Get(legacyKeychainService, accountID); lerr == nil {
		_ = keyring.Set(keychainService, accountID, legacy)
		_ = keyring.Delete(legacyKeychainService, accountID)
		return legacy, nil
	}
	return "", ErrNoSecret
}

func (keychainStore) Delete(accountID string) error {
	err := keyring.Delete(keychainService, accountID)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete app password from keychain: %w", err)
	}
	return nil
}
