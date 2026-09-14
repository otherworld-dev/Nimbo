package account

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLocalAddress(t *testing.T) {
	cases := []struct {
		in, want, wantErr string
	}{
		{"192.168.1.100", "192.168.1.100", ""},
		{"  192.168.1.100:8443 ", "192.168.1.100:8443", ""},
		{"NAS.local", "nas.local", ""},
		{"fd00::1", "[fd00::1]", ""},
		{"[fd00::1]:443", "[fd00::1]:443", ""},
		{"", "", "enter an address"},
		{"https://192.168.1.100", "", "not a URL"},
		{"192.168.1.100/nextcloud", "", "without a path"},
		{"nas:abc", "", "invalid port"},
		{"nas:70000", "", "invalid port"},
		{"cloud.example.com", "", "public address"},
		{"Cloud.Example.com:443", "", "public address"},
		{"192.168.1.100::8443", "", "valid host name"},
		{"nas:abc:def", "", "valid host name"},
		{"nas::", "", "valid host name"},
	}
	for _, c := range cases {
		got, err := ParseLocalAddress(c.in, "cloud.example.com")
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%q: err = %v, want containing %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestLocalRouteJSONRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	st, _ := LoadStore(path)
	with := Account{ID: "a", ServerURL: "https://cloud.example.com", LoginName: "alice",
		Local: &LocalRoute{Address: "192.168.1.100", Pin: "ab12", RootID: "00000042ocabc"}}
	without := Account{ID: "b", ServerURL: "https://other.example.com", LoginName: "bob"}
	if err := st.Upsert(with); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(without); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Count(string(raw), `"local"`) != 1 {
		t.Fatalf("an account without a local route must not write a local key:\n%s", raw)
	}
	st2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := st2.Find("a")
	if a.Local == nil || *a.Local != *with.Local {
		t.Fatalf("local route lost in round trip: %+v", a.Local)
	}
	b, _ := st2.Find("b")
	if b.Local != nil {
		t.Fatalf("account b gained a local route: %+v", b.Local)
	}
}

// A store written before this feature existed loads with Local == nil.
func TestOldStoreLoadsWithoutLocal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	old := `{"accounts":[{"id":"a","serverURL":"https://cloud.example.com","loginName":"alice"}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := st.Find("a")
	if !ok || a.Local != nil {
		t.Fatalf("got %+v, %v", a, ok)
	}
}

// Signing in again replaces the account record wholesale (Upsert), so the
// local route must be carried over — it belongs to the server, not the login.
func TestCompleteKeepsLocalRoute(t *testing.T) {
	SetSecretStore(newFakeSecretStore())
	defer SetSecretStore(keychainStore{})
	path := filepath.Join(t.TempDir(), "accounts.json")
	st, _ := LoadStore(path)
	lr := &LocalRoute{Address: "192.168.1.100", RootID: "00000042ocabc"}
	first := Account{ID: newID("https://cloud.example.com", "alice"),
		ServerURL: "https://cloud.example.com", LoginName: "alice", Local: lr}
	if err := st.Upsert(first); err != nil {
		t.Fatal(err)
	}
	got, err := Complete(st, Credentials{Server: "https://cloud.example.com/", LoginName: "alice", AppPassword: "pw2"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Local == nil || *got.Local != *lr {
		t.Fatalf("Complete dropped the local route: %+v", got.Local)
	}
	stored, _ := st.Find(got.ID)
	if stored.Local == nil || *stored.Local != *lr {
		t.Fatalf("store lost the local route: %+v", stored.Local)
	}
}
