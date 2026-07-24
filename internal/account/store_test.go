package account

import (
	"path/filepath"
	"testing"
)

func TestNewIDStableAndDistinct(t *testing.T) {
	a := newID("https://cloud.example.com", "alice")
	b := newID("https://cloud.example.com/", "alice") // trailing slash differs
	c := newID("https://cloud.example.com", "bob")

	if a == "" {
		t.Fatal("newID returned empty")
	}
	if a != newID("https://cloud.example.com", "alice") {
		t.Error("newID is not stable for identical input")
	}
	if a == c {
		t.Error("different users produced the same id")
	}
	// Trailing slash is part of the raw server string here; ids legitimately
	// differ. The CLI normalises the URL before calling Complete, so this only
	// documents the hashing behaviour.
	_ = b
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")

	st, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore (missing file): %v", err)
	}
	if len(st.Accounts) != 0 {
		t.Fatalf("expected empty store, got %d", len(st.Accounts))
	}

	acc := Account{ID: "id1", ServerURL: "https://cloud.example.com", LoginName: "alice"}
	if err := st.Upsert(acc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Reload from disk and confirm persistence.
	st2, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore (reload): %v", err)
	}
	got, ok := st2.Find("id1")
	if !ok {
		t.Fatal("account not found after reload")
	}
	if got != acc {
		t.Errorf("reloaded account = %+v, want %+v", got, acc)
	}

	// Upsert with same ID replaces rather than duplicates.
	acc.LoginName = "alice2"
	if err := st2.Upsert(acc); err != nil {
		t.Fatalf("Upsert replace: %v", err)
	}
	if len(st2.Accounts) != 1 {
		t.Errorf("expected 1 account after replace, got %d", len(st2.Accounts))
	}

	// Remove deletes it.
	if err := st2.Remove("id1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := st2.Find("id1"); ok {
		t.Error("account still present after Remove")
	}
}

func TestMultiAccountDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	st, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := Account{ID: "ida", ServerURL: "https://a.example.com", LoginName: "alice"}
	b := Account{ID: "idb", ServerURL: "https://b.example.com", LoginName: "bob"}
	for _, acc := range []Account{a, b} {
		if err := st.Upsert(acc); err != nil {
			t.Fatal(err)
		}
	}

	// No DefaultID yet: first account wins (pre-multi-account behaviour).
	if def, ok := st.Default(); !ok || def.ID != "ida" {
		t.Fatalf("default = %+v, want first account", def)
	}

	if err := st.SetDefault("idb"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	if def, _ := st.Default(); def.ID != "idb" {
		t.Errorf("default after SetDefault = %s, want idb", def.ID)
	}

	// Persists across reload.
	st2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if def, _ := st2.Default(); def.ID != "idb" {
		t.Errorf("default after reload = %s, want idb", def.ID)
	}

	// Unknown ID is rejected.
	if err := st2.SetDefault("nope"); err == nil {
		t.Error("SetDefault(nope) succeeded")
	}

	// Removing the default falls back to the remaining account.
	if err := st2.Remove("idb"); err != nil {
		t.Fatal(err)
	}
	if def, ok := st2.Default(); !ok || def.ID != "ida" {
		t.Errorf("default after removing it = %+v, want fallback to ida", def)
	}

	// A stale DefaultID (e.g. hand-edited file) also falls back, not fails.
	st2.DefaultID = "ghost"
	if def, ok := st2.Default(); !ok || def.ID != "ida" {
		t.Errorf("stale DefaultID: default = %+v, want ida", def)
	}
}

func TestDAVRoot(t *testing.T) {
	a := Account{ServerURL: "https://cloud.example.com", LoginName: "alice"}
	want := "https://cloud.example.com/remote.php/dav/files/alice"
	if a.DAVRoot() != want {
		t.Errorf("DAVRoot() = %q, want %q", a.DAVRoot(), want)
	}
}
