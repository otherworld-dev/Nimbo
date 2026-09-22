package account

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// update is Update with a mutate that cannot fail, for tests that only care
// about the resulting store.
func update(t *testing.T, path string, mutate func(*Store)) {
	t.Helper()
	if err := Update(path, func(st *Store) error { mutate(st); return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
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
	update(t, path, func(st *Store) { st.Upsert(acc) })

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
	update(t, path, func(st *Store) { st.Upsert(acc) })
	st3, _ := LoadStore(path)
	if len(st3.Accounts) != 1 || st3.Accounts[0] != acc {
		t.Errorf("after replace: %+v, want just %+v", st3.Accounts, acc)
	}

	// Remove deletes it.
	update(t, path, func(st *Store) { st.Remove("id1") })
	st4, _ := LoadStore(path)
	if _, ok := st4.Find("id1"); ok {
		t.Error("account still present after Remove")
	}
}

func TestMultiAccountDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	a := Account{ID: "ida", ServerURL: "https://a.example.com", LoginName: "alice"}
	b := Account{ID: "idb", ServerURL: "https://b.example.com", LoginName: "bob"}
	update(t, path, func(st *Store) {
		st.Upsert(a)
		st.Upsert(b)
	})

	// No DefaultID yet: first account wins (pre-multi-account behaviour).
	st, _ := LoadStore(path)
	if def, ok := st.Default(); !ok || def.ID != "ida" {
		t.Fatalf("default = %+v, want first account", def)
	}

	// Persists across reload.
	update(t, path, func(st *Store) {
		if err := st.SetDefault("idb"); err != nil {
			t.Fatalf("SetDefault: %v", err)
		}
	})
	st2, _ := LoadStore(path)
	if def, _ := st2.Default(); def.ID != "idb" {
		t.Errorf("default after reload = %s, want idb", def.ID)
	}

	// Unknown ID is rejected, and the failed update leaves the file alone.
	err := Update(path, func(st *Store) error { return st.SetDefault("nope") })
	if err == nil {
		t.Error("SetDefault(nope) succeeded")
	}
	st3, _ := LoadStore(path)
	if def, _ := st3.Default(); def.ID != "idb" {
		t.Errorf("default after a failed update = %s, want idb untouched", def.ID)
	}

	// Removing the default falls back to the remaining account.
	update(t, path, func(st *Store) { st.Remove("idb") })
	st4, _ := LoadStore(path)
	if def, ok := st4.Default(); !ok || def.ID != "ida" {
		t.Errorf("default after removing it = %+v, want fallback to ida", def)
	}

	// A stale DefaultID (e.g. hand-edited file) also falls back, not fails.
	st4.DefaultID = "ghost"
	if def, ok := st4.Default(); !ok || def.ID != "ida" {
		t.Errorf("stale DefaultID: default = %+v, want ida", def)
	}
}

// An update that ends up changing nothing writes nothing: a sign-out with no
// account left, or a switch to the account that is already active, must not
// conjure up an accounts.json where there was none.
func TestUpdateWithoutChangeWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	update(t, path, func(st *Store) { st.Remove("nobody") })
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a no-op update created the store (stat err: %v)", err)
	}
}

func TestDAVRoot(t *testing.T) {
	a := Account{ServerURL: "https://cloud.example.com", LoginName: "alice"}
	want := "https://cloud.example.com/remote.php/dav/files/alice"
	if a.DAVRoot() != want {
		t.Errorf("DAVRoot() = %q, want %q", a.DAVRoot(), want)
	}
}

// Deck #693: every caller used to load its own copy of accounts.json, edit it
// and write the whole file back, so two callers overlapping both started from
// the same copy and whichever saved second threw the other's change away. The
// GUI does this to itself: add an account in the login window while the
// settings window is saving a local route, and one of them silently vanishes.
// Update holds the file's lock across the whole load-edit-save, so every
// change lands. (Two savers of one file were already ordinary before this —
// the fixed "accounts.json.tmp" they once shared made the second save fail
// outright, which internal/atomicfile's unique temp names fixed — so this
// also checks that nothing is left lying beside the store.)
func TestUpdateKeepsEveryConcurrentChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	const n = 32
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- Update(path, func(st *Store) error {
				st.Upsert(Account{
					ID:        fmt.Sprintf("acct-%d", i),
					ServerURL: "https://cloud.example.com",
					LoginName: fmt.Sprintf("user-%d", i),
				})
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Update: %v", err)
		}
	}

	st, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, ok := st.Find(fmt.Sprintf("acct-%d", i)); !ok {
			t.Errorf("acct-%d was lost: another Update overwrote it", i)
		}
	}
	names, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name != path {
			t.Errorf("left behind %s, want only accounts.json", filepath.Base(name))
		}
	}
}
