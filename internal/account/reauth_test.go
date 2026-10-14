package account

import (
	"path/filepath"
	"testing"
)

// Nextcloud takes a login in any case and hands back what was typed, so the
// same account signing in as "test account 2" must be found as the stored
// "Test Account 2", not taken for a new one (GitHub #20, seen on the VM).
func TestFindLoginIgnoresCase(t *testing.T) {
	st := &Store{Accounts: []Account{
		{ID: "a", ServerURL: "https://cloud.example.com", LoginName: "Test Account 2"},
		{ID: "b", ServerURL: "https://other.example.com", LoginName: "Test Account 2"},
	}}
	got, ok := st.FindLogin("https://CLOUD.example.com/", "test account 2")
	if !ok || got.ID != "a" {
		t.Fatalf("FindLogin = %q, %v; want a", got.ID, ok)
	}
	if _, ok := st.FindLogin("https://cloud.example.com", "someone else"); ok {
		t.Fatal("found an account for a different login")
	}
}

// A sign-in in a different case keeps the stored account: same ID, same
// stored login (its folders, state database and keychain entry hang off the
// ID), new password.
func TestCompleteMatchesAnExistingLoginInAnyCase(t *testing.T) {
	SetSecretStore(newFakeSecretStore())
	defer SetSecretStore(keychainStore{})
	path := filepath.Join(t.TempDir(), "accounts.json")
	stored := Account{ID: "4901d3bdf9ca436c", ServerURL: "https://cloud.example.com", LoginName: "Test Account 2"}
	update(t, path, func(st *Store) { st.Upsert(stored) })

	got, err := Complete(path, Credentials{Server: "https://cloud.example.com/", LoginName: "test account 2", AppPassword: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != stored.ID || got.LoginName != stored.LoginName {
		t.Fatalf("Complete made %+v, want the stored account", got)
	}
	st, _ := LoadStore(path)
	if len(st.Accounts) != 1 {
		t.Fatalf("%d accounts, want 1", len(st.Accounts))
	}
	if pw, err := LoadSecret(stored.ID); err != nil || pw != "new" {
		t.Fatalf("password not replaced: %q, %v", pw, err)
	}
}

// Signing a background account in again (GitHub #20) saves its new password
// without making it the shown account, which Complete would.
func TestReauthorizeKeepsTheShownAccount(t *testing.T) {
	SetSecretStore(newFakeSecretStore())
	defer SetSecretStore(keychainStore{})
	path := filepath.Join(t.TempDir(), "accounts.json")
	shown := Account{ID: newID("https://a.example.com", "alice"), ServerURL: "https://a.example.com", LoginName: "alice"}
	other := Account{ID: newID("https://b.example.com", "bob"), ServerURL: "https://b.example.com", LoginName: "bob", Label: "Work"}
	update(t, path, func(st *Store) {
		st.Upsert(shown)
		st.Upsert(other)
		_ = st.SetDefault(shown.ID)
	})

	got, err := Reauthorize(path, Credentials{Server: "https://b.example.com/", LoginName: "bob", AppPassword: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != other.ID || got.Label != "Work" {
		t.Fatalf("Reauthorize returned %+v", got)
	}
	st, _ := LoadStore(path)
	if def, _ := st.Default(); def.ID != shown.ID {
		t.Fatalf("shown account changed to %s", def.LoginName)
	}
	if pw, err := LoadSecret(other.ID); err != nil || pw != "new" {
		t.Fatalf("new password not saved: %q, %v", pw, err)
	}
}

// Complete still makes the account it saves the shown one.
func TestCompleteMakesTheAccountShown(t *testing.T) {
	SetSecretStore(newFakeSecretStore())
	defer SetSecretStore(keychainStore{})
	path := filepath.Join(t.TempDir(), "accounts.json")
	shown := Account{ID: newID("https://a.example.com", "alice"), ServerURL: "https://a.example.com", LoginName: "alice"}
	update(t, path, func(st *Store) { st.Upsert(shown); _ = st.SetDefault(shown.ID) })
	got, err := Complete(path, Credentials{Server: "https://b.example.com", LoginName: "bob", AppPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := LoadStore(path)
	if def, _ := st.Default(); def.ID != got.ID {
		t.Fatalf("shown account is %s, want bob", def.LoginName)
	}
}
