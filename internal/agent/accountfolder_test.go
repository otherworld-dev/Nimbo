package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
)

// Each engine serves one account, so its folder must be that account's alone
// (#11: a second account read, and then mounted, the first account's folder).
func TestBaseDirIsPerAccount(t *testing.T) {
	root := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	a := &Engine{dirs: root.WithAccount("a")}
	b := &Engine{dirs: root.WithAccount("b")}

	if err := a.SetBaseDir(`C:\Users\x\Nextcloud`); err != nil {
		t.Fatal(err)
	}
	if got := a.BaseDir(); got != `C:\Users\x\Nextcloud` {
		t.Fatalf("a.BaseDir() = %q", got)
	}
	if got := a.StoredBaseDir(); got != `C:\Users\x\Nextcloud` {
		t.Fatalf("a.StoredBaseDir() = %q", got)
	}
	if got := b.StoredBaseDir(); got != "" {
		t.Fatalf("b picked up a's folder: %q", got)
	}
	if got := b.BaseDir(); got == `C:\Users\x\Nextcloud` {
		t.Fatalf("b.BaseDir() is a's folder")
	}
}

func TestRememberedPairsArePerAccountAndTakenOnce(t *testing.T) {
	root := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	a := &Engine{dirs: root.WithAccount("a")}
	b := &Engine{dirs: root.WithAccount("b")}
	parked := []config.SyncPair{{LocalDir: `C:\Users\x\Nextcloud`, RemoteRoot: ""}}

	if err := a.ParkPairs(parked); err != nil {
		t.Fatal(err)
	}
	if got := b.TakeRememberedPairs(); len(got) != 0 {
		t.Fatalf("b took a's parked pairs: %v", got)
	}
	if got := a.TakeRememberedPairs(); len(got) != 1 || got[0].LocalDir != parked[0].LocalDir {
		t.Fatalf("a.TakeRememberedPairs() = %v", got)
	}
	if got := a.TakeRememberedPairs(); len(got) != 0 {
		t.Fatalf("pairs restored twice: %v", got)
	}
}

// Parking merges with what is already parked (a pair parked earlier must not
// be lost when more are parked), without duplicating a pair.
func TestParkPairsMergesWithoutDuplicates(t *testing.T) {
	root := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	e := &Engine{dirs: root.WithAccount("a")}
	x := config.SyncPair{LocalDir: `C:\X`, RemoteRoot: "X"}
	y := config.SyncPair{LocalDir: `C:\Y`, RemoteRoot: "Y"}

	if err := e.ParkPairs([]config.SyncPair{x}); err != nil {
		t.Fatal(err)
	}
	if err := e.ParkPairs([]config.SyncPair{y, x}); err != nil {
		t.Fatal(err)
	}
	if got := e.RememberedPairs(); len(got) != 2 {
		t.Fatalf("parked = %v, want x and y once each", got)
	}
}

// Parked pairs come back as real pairs with their excludes, and a whole-account
// one sets the account folder. A pair that can't be added right now (its
// folder can't be created) stays parked instead of being lost; one whose remote
// folder is already synced elsewhere is obsolete and is dropped.
func TestRestoreRememberedPairsBringsBackTheParkedSetup(t *testing.T) {
	e, _ := newHookEngine(t, "http://127.0.0.1:1")
	whole := config.SyncPair{LocalDir: filepath.Join(t.TempDir(), "acct"), RemoteRoot: ""}
	docs := config.SyncPair{LocalDir: filepath.Join(t.TempDir(), "docs"), RemoteRoot: "Docs", Excludes: []string{"Docs/Old"}}
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stuck := config.SyncPair{LocalDir: filepath.Join(blocker, "sub"), RemoteRoot: "Stuck"}
	dupe := config.SyncPair{LocalDir: filepath.Join(t.TempDir(), "dupe"), RemoteRoot: "Docs"}
	if err := e.ParkPairs([]config.SyncPair{whole, docs, stuck, dupe}); err != nil {
		t.Fatal(err)
	}

	got := e.RestoreRememberedPairs()

	if len(got) != 2 {
		t.Fatalf("restored %v, want whole and docs", got)
	}
	pairs, err := e.Pairs()
	if err != nil {
		t.Fatal(err)
	}
	var docsNow *config.SyncPair
	for i := range pairs {
		if pairs[i].LocalDir == docs.LocalDir {
			docsNow = &pairs[i]
		}
	}
	if docsNow == nil || len(docsNow.Excludes) != 1 || docsNow.Excludes[0] != "Docs/Old" {
		t.Fatalf("docs restored without its excludes: %+v", pairs)
	}
	if e.StoredBaseDir() != whole.LocalDir {
		t.Fatalf("account folder = %q, want the whole-account pair %q", e.StoredBaseDir(), whole.LocalDir)
	}
	left := e.RememberedPairs()
	if len(left) != 1 || left[0].LocalDir != stuck.LocalDir {
		t.Fatalf("parked after restore = %v, want only the pair that couldn't be added", left)
	}
}

// A folder another account also uses must not sync (each account would upload
// the other's files). The gate decides per folder, at the time, so the folder
// resumes on its own once the other account has moved away.
func TestPairGateHoldsBackGatedFolders(t *testing.T) {
	e := &Engine{}
	pairs := []Pair{{LocalDir: `C:\Shared`}, {LocalDir: `C:\Mine`, RemoteRoot: "Mine"}}
	if got := e.syncablePairs(pairs); len(got) != 2 {
		t.Fatalf("no gate set: %v", got)
	}
	e.SetPairGate(func(dir string) string {
		if dir == `C:\Shared` {
			return "used by another account"
		}
		return ""
	})
	got := e.syncablePairs(pairs)
	if len(got) != 1 || got[0].LocalDir != `C:\Mine` {
		t.Fatalf("gated = %v", got)
	}
	if why := e.HeldReason(`C:\Shared`); why != "used by another account" {
		t.Fatalf("HeldReason = %q", why)
	}
}

// The guard sweep keeps the state of a folder parked for on-demand mode. It
// must read the parked list of ITS account: the sweep runs per engine.
func TestGuardSweepKeepsThisAccountsParkedFolder(t *testing.T) {
	root := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	e := &Engine{dirs: root.WithAccount("a")}
	live := config.SyncPair{LocalDir: `C:\live`, RemoteRoot: "L"}
	parked := config.SyncPair{LocalDir: `C:\parked`, RemoteRoot: "P"}
	gone := config.SyncPair{LocalDir: `C:\gone`, RemoteRoot: "G"}

	if err := e.dirs.SavePairs([]config.SyncPair{live}); err != nil {
		t.Fatal(err)
	}
	if err := e.ParkPairs([]config.SyncPair{parked}); err != nil {
		t.Fatal(err)
	}
	if err := e.dirs.UpdateGuardState(func(g config.GuardStates) {
		for _, p := range []config.SyncPair{live, parked, gone} {
			g[PairKey(p.LocalDir, p.RemoteRoot)] = config.GuardState{ExemptNext: true}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.reloadGuardState(); err != nil {
		t.Fatal(err)
	}

	e.sweepGuardState()

	got, err := e.dirs.LoadGuardState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[PairKey(parked.LocalDir, parked.RemoteRoot)]; !ok {
		t.Fatalf("the parked folder's guard state was dropped: %v", got)
	}
	if _, ok := got[PairKey(gone.LocalDir, gone.RemoteRoot)]; ok {
		t.Fatalf("a folder that no longer exists kept its guard state: %v", got)
	}
}

// The folder an account last mounted as its virtual-files root is recorded
// per account, so it reconnects to exactly that folder, primary or background.
func TestOnDemandRootIsPerAccount(t *testing.T) {
	root := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	a := &Engine{dirs: root.WithAccount("a")}
	b := &Engine{dirs: root.WithAccount("b")}
	if err := a.SetOnDemandRoot(`C:\A`); err != nil {
		t.Fatal(err)
	}
	if a.OnDemandRoot() != `C:\A` || b.OnDemandRoot() != "" {
		t.Fatalf("a=%q b=%q", a.OnDemandRoot(), b.OnDemandRoot())
	}
}
