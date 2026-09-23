package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/config"
)

// GitHub #11: two accounts ended up syncing one folder. A folder is refused if
// it is another account's folder, sits inside it, or contains it.
func TestFolderClashCatchesEveryOverlap(t *testing.T) {
	others := []accountFolder{{Account: "bob on cloud.example.com", Dir: `C:\Users\x\Nextcloud`}}
	for _, dir := range []string{
		`C:\Users\x\Nextcloud`,
		`c:\users\X\nextcloud`, // Windows paths are case-insensitive
		`C:\Users\x\Nextcloud\Work`,
		`C:\Users\x`,
	} {
		if msg := folderClash(dir, others); msg == "" {
			t.Errorf("%s: not refused", dir)
		} else if !strings.Contains(msg, "bob on cloud.example.com") {
			t.Errorf("%s: message doesn't name the account: %q", dir, msg)
		}
	}
	for _, dir := range []string{`C:\Users\x\Nextcloud2`, `C:\Users\x\Work`, `D:\Nextcloud`} {
		if msg := folderClash(dir, others); msg != "" {
			t.Errorf("%s: refused but doesn't overlap: %q", dir, msg)
		}
	}
}

func TestOtherAccountFoldersReadsEveryOtherAccount(t *testing.T) {
	d := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	st := account.Store{Accounts: []account.Account{
		{ID: "a", ServerURL: "https://one.example.com", LoginName: "amy"},
		{ID: "b", ServerURL: "https://two.example.com", LoginName: "bob"},
		{ID: "c", ServerURL: "https://two.example.com/nc", LoginName: "cat"},
	}}
	_ = d.WithAccount("a").UpdateAccountState(func(s *config.AccountState) { s.BaseDir = `C:\A` })
	_ = d.WithAccount("b").UpdateAccountState(func(s *config.AccountState) { s.BaseDir = `C:\B` })
	_ = d.WithAccount("c").SavePairs([]config.SyncPair{{LocalDir: `C:\C\Photos`, RemoteRoot: "Photos"}})
	_ = d.WithAccount("c").UpdateAccountState(func(s *config.AccountState) {
		s.RememberedPairs = []config.SyncPair{{LocalDir: `C:\C\Parked`, RemoteRoot: "P"}}
	})

	got := otherAccountFolders(d, st, "a")

	dirs := map[string]string{}
	for _, f := range got {
		dirs[f.Dir] = f.Account
	}
	if _, ok := dirs[`C:\A`]; ok {
		t.Fatalf("the excluded account's own folder was listed: %v", got)
	}
	for _, want := range []string{`C:\B`, `C:\C\Photos`, `C:\C\Parked`} {
		if _, ok := dirs[want]; !ok {
			t.Fatalf("%s missing from %v", want, got)
		}
	}
	if dirs[`C:\B`] != "bob on two.example.com" {
		t.Fatalf("label = %q", dirs[`C:\B`])
	}
}

// A new account is offered a folder of its own, never one another account uses.
func TestSuggestAccountFolderAvoidsOtherAccounts(t *testing.T) {
	home := `C:\Users\x`
	if got := suggestAccountFolder(home, "bob", nil, nil); got != filepath.Join(home, "Nextcloud") {
		t.Fatalf("first account: %q", got)
	}
	others := []accountFolder{{Account: "amy", Dir: filepath.Join(home, "Nextcloud")}}
	want := filepath.Join(home, brand.Current.Name+" - bob")
	if got := suggestAccountFolder(home, "bob", others, nil); got != want {
		t.Fatalf("second account: %q, want %q", got, want)
	}
	others = append(others, accountFolder{Account: "old bob", Dir: want})
	if got := suggestAccountFolder(home, "bob", others, nil); got != want+" (2)" {
		t.Fatalf("both taken: %q", got)
	}
}

// When another account's folder contains the home folder (a drive root, the
// home folder itself) nothing under it is free. The search must give up rather
// than loop forever, which hung start-up.
func TestSuggestAccountFolderGivesUpWhenNothingIsFree(t *testing.T) {
	for _, taken := range []string{`C:\Users\x`, `C:\`} {
		others := []accountFolder{{Account: "amy", Dir: taken}}
		if got := suggestAccountFolder(`C:\Users\x`, "bob", others, nil); got != "" {
			t.Fatalf("%s taken: got %q, want no suggestion", taken, got)
		}
	}
}

// A folder that will be mounted without the user choosing it must be missing
// or empty, never a folder that already holds someone's files.
func TestSuggestAccountFolderSkipsUnusableCandidates(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Nextcloud", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := suggestAccountFolder(home, "bob", nil, missingOrEmpty)
	if want := filepath.Join(home, brand.Current.Name+" - bob"); got != want {
		t.Fatalf("got %q, want %q (Nextcloud holds files)", got, want)
	}
}

// What another account is actually SYNCING is what can collide at run time:
// its live pairs in live mode, its mounted root in on-demand mode. Parked pairs
// and a "choose"-mode parent folder sync nothing and must not hold anything back
// (they did, and two accounts then blocked each other for good).
func TestActiveAccountFoldersCountsOnlyWhatSyncs(t *testing.T) {
	d := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	st := account.Store{Accounts: []account.Account{
		{ID: "a", ServerURL: "https://one.example.com", LoginName: "amy"},
		{ID: "b", ServerURL: "https://two.example.com", LoginName: "bob"},
	}}
	b := d.WithAccount("b")
	_ = b.SavePairs([]config.SyncPair{{LocalDir: `C:\B\Photos`, RemoteRoot: "Photos"}})
	_ = b.UpdateAccountState(func(s *config.AccountState) {
		s.BaseDir = `C:\B`
		s.RememberedPairs = []config.SyncPair{{LocalDir: `C:\Parked`, RemoteRoot: "P"}}
	})

	dirs := func(fs []accountFolder) map[string]bool {
		m := map[string]bool{}
		for _, f := range fs {
			m[f.Dir] = true
		}
		return m
	}
	live := dirs(activeAccountFolders(d, st, "a", "live"))
	if !live[`C:\B\Photos`] || live[`C:\B`] || live[`C:\Parked`] {
		t.Fatalf("live mode: %v", live)
	}
	od := dirs(activeAccountFolders(d, st, "a", "ondemand"))
	if !od[`C:\B`] || od[`C:\B\Photos`] || od[`C:\Parked`] {
		t.Fatalf("on-demand mode: %v", od)
	}
}

// Clearing an account's data (sign out and clear, or removing the account)
// takes its folder record too, and nothing of any other account's.
func TestClearSyncDataRemovesTheAccountFolderRecord(t *testing.T) {
	d := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	gone, kept := d.WithAccount("gone"), d.WithAccount("kept")
	for _, ad := range []config.Dirs{gone, kept} {
		if err := ad.UpdateAccountState(func(s *config.AccountState) { s.BaseDir = `C:\X` }); err != nil {
			t.Fatal(err)
		}
	}

	(&App{}).clearSyncData(gone, "gone")

	if _, err := os.Stat(gone.AccountStateFile()); !os.IsNotExist(err) {
		t.Fatalf("the cleared account's folder record survived: %v", err)
	}
	if s, _ := kept.LoadAccountState(); s.BaseDir != `C:\X` {
		t.Fatalf("another account's folder record was touched: %+v", s)
	}
}
