package agent

import (
	"errors"
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

	if err := a.SetRememberedPairs(parked); err != nil {
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

// Parked pairs come back as real pairs, a whole-account one sets the account
// folder, and a pair the caller refuses (it would overlap another account's
// folder) stays parked rather than recreating the overlap or being lost.
func TestRestoreRememberedPairsBringsBackTheParkedSetup(t *testing.T) {
	e, _ := newHookEngine(t, "http://127.0.0.1:1")
	whole := config.SyncPair{LocalDir: filepath.Join(t.TempDir(), "acct"), RemoteRoot: ""}
	docs := config.SyncPair{LocalDir: filepath.Join(t.TempDir(), "docs"), RemoteRoot: "Docs"}
	clash := config.SyncPair{LocalDir: filepath.Join(t.TempDir(), "clash"), RemoteRoot: "Clash"}
	if err := e.SetRememberedPairs([]config.SyncPair{whole, docs, clash}); err != nil {
		t.Fatal(err)
	}

	got := e.RestoreRememberedPairs(func(local string) error {
		if local == clash.LocalDir {
			return errors.New("used by another account")
		}
		return nil
	})

	if len(got) != 2 {
		t.Fatalf("restored %v, want the two allowed pairs", got)
	}
	pairs, err := e.Pairs()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, p := range pairs {
		have[p.LocalDir] = true
	}
	if !have[whole.LocalDir] || !have[docs.LocalDir] || have[clash.LocalDir] {
		t.Fatalf("pairs after restore = %v", pairs)
	}
	if e.StoredBaseDir() != whole.LocalDir {
		t.Fatalf("account folder = %q, want the whole-account pair %q", e.StoredBaseDir(), whole.LocalDir)
	}
	if left := e.TakeRememberedPairs(); len(left) != 1 || left[0].LocalDir != clash.LocalDir {
		t.Fatalf("parked after restore = %v, want only the refused pair", left)
	}
}

// An install already in the #11 state has a pair overlapping another account's
// folder. It is parked (not synced, setup kept) until the overlap is gone.
func TestHoldBackPairsParksTheOverlappingFolders(t *testing.T) {
	e, _ := newHookEngine(t, "http://127.0.0.1:1")
	shared := config.SyncPair{LocalDir: filepath.Join(t.TempDir(), "shared"), RemoteRoot: ""}
	mine := config.SyncPair{LocalDir: filepath.Join(t.TempDir(), "mine"), RemoteRoot: "Mine"}
	if err := e.dirs.SavePairs([]config.SyncPair{shared, mine}); err != nil {
		t.Fatal(err)
	}

	held := e.HoldBackPairs(func(local string) bool { return local == shared.LocalDir })

	if len(held) != 1 || held[0].LocalDir != shared.LocalDir {
		t.Fatalf("held = %v", held)
	}
	pairs, _ := e.Pairs()
	if len(pairs) != 1 || pairs[0].LocalDir != mine.LocalDir {
		t.Fatalf("pairs left = %v", pairs)
	}
	if parked := e.RememberedPairs(); len(parked) != 1 || parked[0].LocalDir != shared.LocalDir {
		t.Fatalf("parked = %v", parked)
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
	if err := e.SetRememberedPairs([]config.SyncPair{parked}); err != nil {
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
