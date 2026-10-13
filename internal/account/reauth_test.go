package account

import (
	"path/filepath"
	"testing"
)

// IDFor is the ID a sign-in to that server and login would get, so the GUI can
// tell an account it already has from a new one before saving anything.
func TestIDFor(t *testing.T) {
	want := newID("https://cloud.example.com", "alice")
	if got := IDFor("https://cloud.example.com/", "alice"); got != want {
		t.Fatalf("IDFor with a trailing slash = %q, want %q", got, want)
	}
	if IDFor("https://cloud.example.com", "bob") == want {
		t.Fatal("a different login got the same ID")
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
