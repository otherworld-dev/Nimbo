package mobile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/config"
)

// noopListener satisfies Listener for tests that never expect callbacks.
type noopListener struct{}

func (noopListener) OnStatus(string)                     {}
func (noopListener) OnProgress(string)                   {}
func (noopListener) OnToast(string, string, string)      {}
func (noopListener) OnAuthLost()                         {}
func (noopListener) OnPairSynced(string, string, string) {}
func (noopListener) OnNotificationsChanged(int)          {}
func (noopListener) OnConflictsChanged()                 {}
func (noopListener) OnPauseChanged()                     {}

// gomobile generates a public no-arg constructor for LoginFlow (no NewLoginFlow
// suppresses it), so Kotlin can hold a zero-value instance — its methods must
// degrade gracefully instead of nil-panicking the process.
func TestLoginFlowZeroValueSafe(t *testing.T) {
	lf := &LoginFlow{}
	if got := lf.URL(); got != "" {
		t.Errorf("zero LoginFlow.URL() = %q, want \"\"", got)
	}
	lf.Cancel() // must not panic
	if _, err := lf.Poll(); err == nil {
		t.Error("zero LoginFlow.Poll() must return an error, not panic")
	}
}

func TestStartNilListenerRejected(t *testing.T) {
	c := &Client{}
	err := c.Start(nil)
	if err == nil || !strings.Contains(err.Error(), "listener") {
		t.Fatalf("Start(nil) must fail fast naming the listener (a Java null crossing gomobile is a nil interface); got: %v", err)
	}
}

func TestStartWhileRunningRejected(t *testing.T) {
	c := &Client{starting: true} // engine bring-up in progress
	err := c.Start(noopListener{})
	if err == nil {
		t.Fatal("Start while running must return an error — a silent nil leaves the new listener unwired forever")
	}
}

func TestMarshalSliceNilIsEmptyArray(t *testing.T) {
	got, err := marshalSlice[int](nil)
	if err != nil || got != "[]" {
		t.Fatalf(`marshalSlice(nil) = %q, %v; want "[]", nil — Kotlin's JSONArray("null") throws`, got, err)
	}
	got, err = marshalSlice([]int{1, 2})
	if err != nil || got != "[1,2]" {
		t.Fatalf("marshalSlice([1,2]) = %q, %v", got, err)
	}
}

func TestIsUnder(t *testing.T) {
	root := filepath.Join("data", "app")
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join("data", "app", "Nextcloud"), true},
		{filepath.Join("data", "app"), true},
		{filepath.Join("data", "other"), false},
		{filepath.Join("data", "approximate"), false}, // prefix of the string, not the path
		{filepath.Join("storage", "emulated", "0", "Nimbo"), false},
	}
	for _, c := range cases {
		if got := isUnder(c.path, root); got != c.want {
			t.Errorf("isUnder(%q, %q) = %v, want %v", c.path, root, got, c.want)
		}
	}
	if isUnder("anything", "") {
		t.Error("empty root must never match (zero-value Client)")
	}
}

// redirectConfig points config.Resolve at a temp dir for every platform.
func redirectConfig(t *testing.T) config.Dirs {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("APPDATA", filepath.Join(tmp, "roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "local"))
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	d, err := config.Resolve()
	if err != nil {
		t.Fatalf("config.Resolve: %v", err)
	}
	return d
}

// erroringDeleteStore simulates an Android Keystore whose Delete always fails
// (missing alias after a backup restore, corrupted keyset, …).
type erroringDeleteStore struct{}

func (erroringDeleteStore) Get(string) (string, error) { return "", account.ErrNoSecret }
func (erroringDeleteStore) Set(string, string) error   { return nil }
func (erroringDeleteStore) Delete(string) error        { return errors.New("keystore: alias not found") }

func TestLogoutRemovesAccountWhenSecretDeleteFails(t *testing.T) {
	d := redirectConfig(t)
	if err := os.MkdirAll(filepath.Dir(d.AccountsFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(account.Account{ID: "acct1", ServerURL: "https://x", LoginName: "adam"}); err != nil {
		t.Fatal(err)
	}

	account.SetSecretStore(erroringDeleteStore{})
	c := &Client{}
	if err := c.Logout("acct1"); err != nil {
		t.Fatalf("Logout must succeed despite a secret-store Delete error (desktop treats it as best-effort); got: %v", err)
	}
	st2, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st2.Find("acct1"); ok {
		t.Fatal("account must be removed from the store by Logout")
	}
}
