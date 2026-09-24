package main

import (
	"os"
	"os/exec"
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
		{CLSID: "{CCCC}", Target: `c:\byPATH\`},
		{CLSID: "{BBBB}", Target: `C:\Gone`},
	}
	roots := []cfapi.ShellSyncRoot{
		{Path: `C:\Live`, NamespaceCLSID: "{aaaa}"},
		{Path: `C:\ByPath`, NamespaceCLSID: "{DDDD}"},
	}
	got := orphanNavNodes(nodes, roots)
	if len(got) != 1 || got[0].CLSID != "{BBBB}" {
		t.Fatalf("got %v", got)
	}
}

// Where Windows hasn't recorded (or not yet) which sidebar entry belongs to a
// root, nothing can be told apart safely: no entry is removed at all.
func TestOrphanNavNodesDoesNothingWithoutNamespaceIDs(t *testing.T) {
	nodes := []shellns.NavNode{{CLSID: "{BBBB}", Target: `C:\Gone`}}
	roots := []cfapi.ShellSyncRoot{{Path: `C:\Live`}}
	if got := orphanNavNodes(nodes, roots); len(got) != 0 {
		t.Fatalf("removed entries while a root's entry ID is unknown: %v", got)
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
		name                       string
		exists, parent, registered bool
		since                      time.Time
		want                       missingAction
		keepSince                  bool
	}{
		{"there", true, true, true, time.Time{}, mountRoot, false},
		{"never registered", false, true, false, time.Time{}, mountRoot, false},
		{"just vanished", false, true, true, time.Time{}, waitForRoot, true},
		{"still within a day", false, true, true, now.Add(-2 * time.Hour), waitForRoot, true},
		{"gone for a day", false, true, true, now.Add(-25 * time.Hour), recreateRoot, false},
		// The whole drive or parent folder is away (a USB disk, a locked
		// BitLocker volume, another disk now using its letter): never
		// re-create, and restart the day once it is back rather than
		// counting the time it was away.
		{"drive away for days", false, false, true, now.Add(-72 * time.Hour), waitForRoot, false},
		{"drive away, never seen missing", false, false, true, time.Time{}, waitForRoot, false},
	}
	for _, c := range cases {
		got, since := missingRootAction(c.exists, c.parent, c.registered, c.since, now)
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
// is only mounted without the user choosing it for a single-account install.
func TestUnchosenUsableLimitsRegisteredRoots(t *testing.T) {
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	registered := func(string) bool { return true }
	if !unchosenUsable(registered, true)(full) {
		t.Error("single account: its registered root was refused")
	}
	// With several accounts even a folder with this account's own default
	// name may be another's: the same login on a different server.
	if unchosenUsable(registered, false)(full) {
		t.Error("several accounts: a registered root was accepted")
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

// The same folder can be written more than one way (a junction, a short name,
// a \?\ prefix); before unregistering, folders are compared by identity.
func TestSameFolderAsAny(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, dir).CombinedOutput(); err != nil {
		t.Skipf("no junction: %v %s", err, out)
	}
	if !sameFolderAsAny(link, []string{dir}) {
		t.Fatal("a junction to a claimed folder was not recognised")
	}
	if sameFolderAsAny(t.TempDir(), []string{dir}) {
		t.Fatal("a different folder was taken for the same one")
	}
}

// The volume a folder is on is identified, so a different disk given the same
// drive letter isn't mistaken for the one the root lived on.
func TestVolumeIDIdentifiesTheDisk(t *testing.T) {
	dir := t.TempDir()
	id := volumeID(dir)
	if id == "" {
		t.Skip("no volume id on this system")
	}
	if volumeID(filepath.Join(dir, "missing", "deeper")) != id {
		t.Fatal("a path that doesn't exist yet on the same disk got a different id")
	}
}
