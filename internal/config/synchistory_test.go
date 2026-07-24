package config

import (
	"os"
	"testing"
)

func TestSyncHistoryRoundtrip(t *testing.T) {
	d := Dirs{Config: t.TempDir()}.WithAccount("acct1")

	hist, err := d.LoadSyncHistory()
	if err != nil || len(hist) != 0 {
		t.Fatalf("missing file: got %v, %v; want empty, nil", hist, err)
	}
	if err := d.MarkPairSynced("pk1"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkPairSynced("pk2"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkPairSynced("pk1"); err != nil { // idempotent
		t.Fatal(err)
	}
	hist, err = d.LoadSyncHistory()
	if err != nil || !hist["pk1"] || !hist["pk2"] || len(hist) != 2 {
		t.Fatalf("roundtrip: got %v, %v", hist, err)
	}
	// Scoped like PairsFile: another account starts empty.
	other, err := Dirs{Config: d.Config}.WithAccount("acct2").LoadSyncHistory()
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-account leak: %v, %v", other, err)
	}
}

func TestSyncHistoryCorruptFile(t *testing.T) {
	d := Dirs{Config: t.TempDir()}.WithAccount("a")
	if err := os.WriteFile(d.SyncHistoryFile(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.LoadSyncHistory(); err == nil {
		t.Fatal("corrupt file: want error")
	}
	// MarkPairSynced self-heals: rebuilds the file instead of wedging forever.
	if err := d.MarkPairSynced("pk1"); err != nil {
		t.Fatalf("self-heal write: %v", err)
	}
	hist, err := d.LoadSyncHistory()
	if err != nil || !hist["pk1"] {
		t.Fatalf("after self-heal: %v %v", hist, err)
	}
}

func TestActiveAppDirDevSuffix(t *testing.T) {
	orig := hasPackageIdentity
	defer func() { hasPackageIdentity = orig }()

	hasPackageIdentity = func() bool { return true }
	if got := activeAppDir(); got != appDir {
		t.Fatalf("packaged: got %q, want %q", got, appDir)
	}
	hasPackageIdentity = func() bool { return false }
	if got := activeAppDir(); got != appDir+"-dev" {
		t.Fatalf("unpackaged: got %q, want %q", got, appDir+"-dev")
	}
}
