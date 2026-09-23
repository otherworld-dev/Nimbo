package config

import (
	"path/filepath"
	"testing"
)

// Two accounts must never share a folder setting: #11 was a second account
// being offered, and then mounting, the first account's folder because the
// folder lived in the one global settings file.
func TestAccountFolderIsPerAccount(t *testing.T) {
	root := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	a, b := root.WithAccount("a"), root.WithAccount("b")

	if err := a.UpdateAccountState(func(s *AccountState) {
		s.BaseDir = `C:\Users\x\Nextcloud`
		s.RememberedPairs = []SyncPair{{LocalDir: `C:\Users\x\Nextcloud\Photos`, RemoteRoot: "Photos"}}
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateAccountState(func(s *AccountState) { s.BaseDir = `C:\Users\x\Work` }); err != nil {
		t.Fatal(err)
	}

	sa, err := a.LoadAccountState()
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.LoadAccountState()
	if err != nil {
		t.Fatal(err)
	}
	if sa.BaseDir != `C:\Users\x\Nextcloud` || len(sa.RememberedPairs) != 1 {
		t.Fatalf("account a = %+v", sa)
	}
	if sb.BaseDir != `C:\Users\x\Work` || len(sb.RememberedPairs) != 0 {
		t.Fatalf("account b = %+v", sb)
	}
	if filepath.Dir(a.AccountStateFile()) != root.Config || a.AccountStateFile() == b.AccountStateFile() {
		t.Fatalf("state files not per account: %q %q", a.AccountStateFile(), b.AccountStateFile())
	}
}

func TestAccountStateMissingFileIsEmpty(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}.WithAccount("a")
	s, err := d.LoadAccountState()
	if err != nil || s.BaseDir != "" || s.RememberedPairs != nil {
		t.Fatalf("got %+v, %v", s, err)
	}
}

// The folder used to be global; on upgrade it belongs to the account that was
// active, and is then cleared so no other account can pick it up.
func TestMigrateAccountFolderMovesTheGlobalFolderOnce(t *testing.T) {
	root := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	if err := root.SaveSettings(Settings{
		BaseDir:         `C:\Users\x\Nextcloud`,
		RememberedPairs: []SyncPair{{LocalDir: `C:\Users\x\Nextcloud`, RemoteRoot: ""}},
		Theme:           "dark",
	}); err != nil {
		t.Fatal(err)
	}
	a, b := root.WithAccount("a"), root.WithAccount("b")

	a.MigrateAccountFolder([]string{"b"})
	b.MigrateAccountFolder([]string{"a"}) // nothing left to take

	sa, _ := a.LoadAccountState()
	sb, _ := b.LoadAccountState()
	if sa.BaseDir != `C:\Users\x\Nextcloud` || len(sa.RememberedPairs) != 1 {
		t.Fatalf("account a did not receive the global folder: %+v", sa)
	}
	if sb.BaseDir != "" || len(sb.RememberedPairs) != 0 {
		t.Fatalf("account b picked up a folder: %+v", sb)
	}
	g, _ := root.LoadSettings()
	if g.BaseDir != "" || len(g.RememberedPairs) != 0 {
		t.Fatalf("global folder not cleared: %+v", g)
	}
	if g.Theme != "dark" {
		t.Fatalf("migration clobbered other settings: theme=%q", g.Theme)
	}
}

// An account that already has its own folder keeps it; the global value is
// still cleared so it can't leak to another account later.
func TestMigrateAccountFolderKeepsAnExistingAccountFolder(t *testing.T) {
	root := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	a := root.WithAccount("a")
	_ = a.UpdateAccountState(func(s *AccountState) { s.BaseDir = `D:\Mine` })
	_ = root.SaveSettings(Settings{BaseDir: `C:\Users\x\Nextcloud`})

	a.MigrateAccountFolder(nil)

	sa, _ := a.LoadAccountState()
	if sa.BaseDir != `D:\Mine` {
		t.Fatalf("existing account folder overwritten: %q", sa.BaseDir)
	}
	if g, _ := root.LoadSettings(); g.BaseDir != "" {
		t.Fatalf("global folder not cleared: %q", g.BaseDir)
	}
}

// The global folder was written by whichever account last ran setup, which is
// not always the default one. A folder holding another account's sync folders
// (and none of the default account's) belongs to that other account, so the
// default account must not claim it; it gets no folder and picks one later.
func TestMigrateAccountFolderSkipsAnotherAccountsFolder(t *testing.T) {
	root := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	_ = root.SaveSettings(Settings{BaseDir: `D:\B`})
	a, b := root.WithAccount("a"), root.WithAccount("b")
	_ = a.SavePairs([]SyncPair{{LocalDir: `C:\Users\x\Nextcloud\Photos`, RemoteRoot: "Photos"}})
	_ = b.SavePairs([]SyncPair{{LocalDir: `D:\B\Work`, RemoteRoot: "Work"}})

	a.MigrateAccountFolder([]string{"b"})

	if sa, _ := a.LoadAccountState(); sa.BaseDir != "" {
		t.Fatalf("default account claimed another account's folder: %q", sa.BaseDir)
	}
	if g, _ := root.LoadSettings(); g.BaseDir != "" {
		t.Fatalf("global folder not cleared: %q", g.BaseDir)
	}
}

// The default account's own folder still migrates when the other accounts'
// folders are elsewhere.
func TestMigrateAccountFolderKeepsTheDefaultsOwnFolder(t *testing.T) {
	root := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	_ = root.SaveSettings(Settings{BaseDir: `C:\Users\x\Nextcloud`})
	a, b := root.WithAccount("a"), root.WithAccount("b")
	_ = a.SavePairs([]SyncPair{{LocalDir: `C:\Users\x\Nextcloud`, RemoteRoot: ""}})
	_ = b.SavePairs([]SyncPair{{LocalDir: `D:\B`, RemoteRoot: ""}})

	a.MigrateAccountFolder([]string{"b"})

	if sa, _ := a.LoadAccountState(); sa.BaseDir != `C:\Users\x\Nextcloud` {
		t.Fatalf("default account lost its folder: %q", sa.BaseDir)
	}
}
