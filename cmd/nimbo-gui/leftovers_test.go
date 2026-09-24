package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/shellns"
)

// A registered root is a leftover when nothing mounts it now and no configured
// account claims its folder (GitHub #10: roots left by an older version, or by
// an account since removed, stay in the Explorer sidebar for good).
func TestLeftoverRoots(t *testing.T) {
	roots := []cfapi.ShellSyncRoot{
		{ID: "mounted", Path: `C:\Users\x\Nextcloud`},
		{ID: "claimed", Path: `C:\Users\x\Nimbo - bob`},
		{ID: "leftover", Path: `C:\Users\x\Nextcloud-Nimbo`},
	}
	mounted := map[string]bool{`C:\Users\x\Nextcloud`: true}
	claimed := []string{`c:\users\x\nimbo - BOB`}
	got := leftoverRoots(roots, claimed, mounted)
	if len(got) != 1 || got[0].ID != "leftover" {
		t.Fatalf("got %v", got)
	}
}

// A sidebar entry with Nimbo's icon that belongs to no registered sync root is
// an orphan (the entries the reporter was left with). Entries of registered
// roots stay, whoever registered them.
func TestOrphanNavNodes(t *testing.T) {
	nodes := []shellns.NavNode{
		{CLSID: "{AAAA}", Target: `C:\Live`},
		{CLSID: "{BBBB}", Target: `C:\Gone`},
	}
	live := map[string]bool{"{aaaa}": true}
	got := orphanNavNodes(nodes, live)
	if len(got) != 1 || got[0].CLSID != "{BBBB}" {
		t.Fatalf("got %v", got)
	}
}

// Only roots this kind of build registered are swept: the installed app owns
// roots whose icon is in its package family (any version), a development build
// owns roots whose icon is its own executable. Neither may unregister the
// other's roots, which it has no account configuration for.
func TestRootOwnedBy(t *testing.T) {
	pkgExe := `C:\Program Files\WindowsApps\Nimbo_0.1.8.321_x64__hthxzjrt90bbe\nimbo-gui.exe`
	for icon, want := range map[string]bool{
		`C:\Program Files\WindowsApps\Nimbo_0.1.0.267_x64__hthxzjrt90bbe\nimbo-gui.exe,0`:          true,
		`C:\Program Files\WindowsApps\Nimbo_0.1.8.321_x64__hthxzjrt90bbe\nimbo-gui.exe,0`:          true,
		`C:\Program Files\WindowsApps\OtherworldDev.Nimbo_0.1.6.0_x64__8sdes0k5tqray\nimbo-gui.exe,0`: false,
		`E:\Git\Nimbo\bin\nimbo-gui.exe,0`: false,
		``:                                  false,
	} {
		if got := rootOwnedBy(icon, pkgExe); got != want {
			t.Errorf("packaged, icon %q: got %v", icon, got)
		}
	}
	devExe := `E:\Git\Nimbo\bin\nimbo-gui.exe`
	if !rootOwnedBy(`E:\Git\Nimbo\bin\nimbo-gui.exe,0`, devExe) {
		t.Error("dev build doesn't own its own root")
	}
	if rootOwnedBy(`C:\Program Files\WindowsApps\Nimbo_0.1.8.321_x64__hthxzjrt90bbe\nimbo-gui.exe,0`, devExe) {
		t.Error("dev build owns the installed app's root")
	}
}

// A registered root whose folder has vanished was most likely renamed or moved
// (the reporter renamed his). Re-creating it straight away strands the moved
// copy's files for good, so Nimbo waits a day for the folder to come back
// before it gives up and sets the folder up again.
func TestMissingRootAction(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name               string
		exists, registered bool
		since              time.Time
		want               missingAction
		keepSince          bool
	}{
		{"there", true, true, time.Time{}, mountRoot, false},
		{"never registered", false, false, time.Time{}, mountRoot, false},
		{"just vanished", false, true, time.Time{}, waitForRoot, true},
		{"still within a day", false, true, now.Add(-2 * time.Hour), waitForRoot, true},
		{"gone for a day", false, true, now.Add(-25 * time.Hour), recreateRoot, false},
	}
	for _, c := range cases {
		got, since := missingRootAction(c.exists, c.registered, c.since, now)
		if got != c.want {
			t.Errorf("%s: action %v, want %v", c.name, got, c.want)
		}
		if c.keepSince != !since.IsZero() {
			t.Errorf("%s: since = %v", c.name, since)
		}
		if c.name == "still within a day" && !since.Equal(c.since) {
			t.Errorf("%s: the first sighting was reset to %v", c.name, since)
		}
	}
}

// Keeping a registration is always the safe direction: a root that holds a
// folder some account claims (a parent with that account's parked folders in
// it) is kept even though no claim names the root itself.
func TestAbandonedRootsKeepsARootHoldingAClaim(t *testing.T) {
	got := abandonedRoots([]string{`C:\Parent`}, nil, []string{`C:\Parent\Pair`})
	if len(got) != 0 {
		t.Fatalf("unregistered a root holding a claimed folder: %v", got)
	}
}

// Unregistering flattens a folder, so it must never happen on a guess: when
// any account's folder setup can't be read, nothing counts as unclaimed.
func TestClaimedFoldersStrictRefusesUnreadableState(t *testing.T) {
	d := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	st := account.Store{Accounts: []account.Account{{ID: "a"}, {ID: "b"}}}
	_ = d.WithAccount("a").UpdateAccountState(func(s *config.AccountState) { s.BaseDir = `C:\A` })
	if _, ok := claimedFoldersStrict(d, st); !ok {
		t.Fatal("readable state refused")
	}
	if err := os.WriteFile(d.WithAccount("b").AccountStateFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := claimedFoldersStrict(d, st); ok {
		t.Fatal("an unreadable account state was treated as claiming nothing")
	}
}

// A folder that is already a Nimbo root may hold another account's files. It
// is only mounted without the user choosing it for a single-account install,
// or when it is this account's own "<brand> - <login>" folder.
func TestUnchosenUsableLimitsRegisteredRoots(t *testing.T) {
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	registered := func(string) bool { return true }
	if !unchosenUsable(registered, true, "")(full) {
		t.Error("single account: its registered root was refused")
	}
	if unchosenUsable(registered, false, "")(full) {
		t.Error("several accounts: another root was accepted")
	}
	if !unchosenUsable(registered, false, full)(full) {
		t.Error("several accounts: the account's own folder was refused")
	}
}

// One order decides an account's virtual-files root wherever it is mounted:
// its whole-account pair, then the root it last mounted, then its folder.
func TestOnDemandRootOrder(t *testing.T) {
	if got := onDemandRootOrder(`C:\Pair`, `C:\Mounted`, `C:\Base`); got != `C:\Pair` {
		t.Errorf("got %q", got)
	}
	if got := onDemandRootOrder("", `C:\Mounted`, `C:\Base`); got != `C:\Mounted` {
		t.Errorf("got %q", got)
	}
	if got := onDemandRootOrder("", "", `C:\Base`); got != `C:\Base` {
		t.Errorf("got %q", got)
	}
}
