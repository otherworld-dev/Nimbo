package account

import (
	"errors"
	"testing"
)

// fakeSecretStore implements SecretStore in memory, honouring the interface
// contract (ErrNoSecret for absent Get, nil Delete when absent).
type fakeSecretStore struct{ m map[string]string }

func newFakeSecretStore() *fakeSecretStore { return &fakeSecretStore{m: map[string]string{}} }

func (f *fakeSecretStore) Get(id string) (string, error) {
	v, ok := f.m[id]
	if !ok {
		return "", ErrNoSecret
	}
	return v, nil
}
func (f *fakeSecretStore) Set(id, secret string) error { f.m[id] = secret; return nil }
func (f *fakeSecretStore) Delete(id string) error      { delete(f.m, id); return nil }

func TestSetSecretStoreRoutesOperations(t *testing.T) {
	fake := newFakeSecretStore()
	SetSecretStore(fake)
	defer SetSecretStore(keychainStore{})

	if err := SaveSecret("acct", "pw"); err != nil {
		t.Fatalf("SaveSecret: %v", err)
	}
	got, err := LoadSecret("acct")
	if err != nil || got != "pw" {
		t.Fatalf("LoadSecret = %q, %v; want \"pw\", nil", got, err)
	}
	if err := DeleteSecret("acct"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if _, err := LoadSecret("acct"); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("after delete want ErrNoSecret, got %v", err)
	}
}

func TestSetSecretStoreNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("SetSecretStore(nil) must panic: a nil store would turn every later secret operation into a nil-interface panic at some arbitrary call site")
		}
	}()
	SetSecretStore(nil)
}
