package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Every saver used to build its temp path as a FIXED "<file>.tmp", so two
// concurrent writers shared one temp file: the first os.Rename consumed it and
// the second failed with ENOENT, silently dropping its write. Hit for real on
// Android 2026-08-12 when mobile.Client.SetBaseDir raced the engine's own
// startup save; nothing about it is Android-specific.
func TestConcurrentSavesDoNotCollide(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}

	const n = 32
	errs := make(chan error, n*4)
	var wg sync.WaitGroup
	save := func(fn func(i int) error, i int) {
		defer wg.Done()
		if err := fn(i); err != nil {
			errs <- err
		}
	}
	for i := 0; i < n; i++ {
		wg.Add(4)
		go save(func(i int) error { return d.SaveSettings(Settings{BaseDir: fmt.Sprintf("/base/%d", i)}) }, i)
		go save(func(i int) error {
			return d.SavePairs([]SyncPair{{LocalDir: fmt.Sprintf("/local/%d", i), RemoteRoot: "/remote"}})
		}, i)
		go save(func(i int) error { return d.SaveIgnore([]string{fmt.Sprintf("pattern-%d", i)}) }, i)
		go save(func(i int) error {
			return d.SaveHeldLocks([]HeldLock{{Account: "adam", RemotePath: fmt.Sprintf("f-%d.txt", i)}})
		}, i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent save failed: %v", err)
	}

	// Whichever writer won, every file must be a complete document — a reader
	// must never see a half-written or absent one.
	s, err := d.LoadSettings()
	if err != nil || !strings.HasPrefix(s.BaseDir, "/base/") {
		t.Errorf("settings after concurrent saves: %+v, err %v", s, err)
	}
	pairs, err := d.LoadPairs()
	if err != nil || len(pairs) != 1 || !strings.HasPrefix(pairs[0].LocalDir, "/local/") {
		t.Errorf("pairs after concurrent saves: %v, err %v", pairs, err)
	}
	pats, err := d.LoadIgnore()
	if err != nil || len(pats) != 1 || !strings.HasPrefix(pats[0], "pattern-") {
		t.Errorf("ignore after concurrent saves: %v, err %v", pats, err)
	}
	locks, err := d.LoadHeldLocks()
	if err != nil || len(locks) != 1 {
		t.Errorf("held locks after concurrent saves: %v, err %v", locks, err)
	}

	// And no temp file may be left lying around in the config directory.
	if stray := strayTempFiles(t, d.Config); len(stray) != 0 {
		t.Errorf("left temp files behind: %v", stray)
	}
}

// A save into a config directory that doesn't exist yet must create it rather
// than fail — the mobile host hands us a directory it hasn't made.
func TestSavesCreateMissingConfigDir(t *testing.T) {
	for name, save := range map[string]func(d Dirs) error{
		"settings":  func(d Dirs) error { return d.SaveSettings(Settings{BaseDir: "/sync"}) },
		"pairs":     func(d Dirs) error { return d.SavePairs([]SyncPair{{LocalDir: "/l", RemoteRoot: "/r"}}) },
		"ignore":    func(d Dirs) error { return d.SaveIgnore([]string{"*.log"}) },
		"locks":     func(d Dirs) error { return d.SaveHeldLocks([]HeldLock{{Account: "adam"}}) },
		"blacklist": func(d Dirs) error { return d.AddBlacklist(`C:\bad\.htaccess`) },
		"history":   func(d Dirs) error { return d.MarkPairSynced("pair-1") },
	} {
		t.Run(name, func(t *testing.T) {
			d := Dirs{Config: filepath.Join(t.TempDir(), "config", "nimbo"), Data: t.TempDir()}
			if err := save(d); err != nil {
				t.Fatalf("save into a not-yet-created config dir: %v", err)
			}
		})
	}
}

// A write that cannot be committed must not leave its temp file behind. With a
// unique temp name per write, litter would otherwise accumulate forever.
func TestFailedSaveLeavesNoTempFile(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	// A directory sitting where the settings file belongs: the rename can't win.
	if err := os.Mkdir(d.SettingsFile(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveSettings(Settings{BaseDir: "/sync"}); err == nil {
		t.Fatal("SaveSettings should fail when the target path is a directory")
	}
	if stray := strayTempFiles(t, d.Config); len(stray) != 0 {
		t.Errorf("failed save left temp files behind: %v", stray)
	}
}

// A safe atomic write still isn't enough for the load-mutate-save that every
// settings change does: two of them read the same file, so the second save
// writes back the stale value it read and the first change is gone. Settings are
// written from the GUI's handler goroutines AND from the engine's, so this has
// to hold under concurrency, not just not crash.
func TestConcurrentSettingsUpdatesKeepEveryField(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}

	set := []func(*Settings){
		func(s *Settings) { s.BaseDir = "/sync" },
		func(s *Settings) { s.UploadKBps = 512 },
		func(s *Settings) { s.DownloadKBps = 1024 },
		func(s *Settings) { s.Theme = "dark" },
		func(s *Settings) { s.SyncMode = "ondemand" },
		func(s *Settings) { s.ConflictPolicy = "keepboth" },
		func(s *Settings) { s.BetaUpdates = true },
		func(s *Settings) { s.FileLocking = true },
		func(s *Settings) { s.PinnedApps = []string{"files"} },
	}
	var wg sync.WaitGroup
	for _, mutate := range set {
		wg.Add(1)
		go func(mutate func(*Settings)) {
			defer wg.Done()
			if err := d.UpdateSettings(mutate); err != nil {
				t.Errorf("UpdateSettings: %v", err)
			}
		}(mutate)
	}
	wg.Wait()

	got, err := d.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	want := Settings{
		BaseDir: "/sync", UploadKBps: 512, DownloadKBps: 1024, Theme: "dark",
		SyncMode: "ondemand", ConflictPolicy: "keepboth", BetaUpdates: true,
		FileLocking: true, PinnedApps: []string{"files"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("concurrent updates lost fields:\n got  %+v\n want %+v", got, want)
	}
}

// MarkPairSynced is the same load-mutate-save, and the engine calls it once per
// pair as each finishes its initial clone. A lost marker isn't cosmetic: the
// file is the tripwire that tells a state reset apart from a first run.
func TestConcurrentMarkPairSyncedKeepsEveryKey(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := d.MarkPairSynced(fmt.Sprintf("pair-%d", i)); err != nil {
				t.Errorf("MarkPairSynced: %v", err)
			}
		}(i)
	}
	wg.Wait()

	hist, err := d.LoadSyncHistory()
	if err != nil {
		t.Fatalf("LoadSyncHistory: %v", err)
	}
	for i := 0; i < n; i++ {
		if !hist[fmt.Sprintf("pair-%d", i)] {
			t.Errorf("pair-%d missing from history (%d of %d recorded)", i, len(hist), n)
		}
	}
}

// Blacklisting is load-mutate-save too, and the engine adds entries as it walks
// into forbidden names — several in the same pass.
func TestConcurrentBlacklistAddsKeepEveryPath(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := d.AddBlacklist(fmt.Sprintf(`C:\sync\dir%d\.htaccess`, i)); err != nil {
				t.Errorf("AddBlacklist: %v", err)
			}
		}(i)
	}
	wg.Wait()

	set, err := d.LoadBlacklist()
	if err != nil {
		t.Fatalf("LoadBlacklist: %v", err)
	}
	for i := 0; i < n; i++ {
		if !set[PathKey(fmt.Sprintf(`C:\sync\dir%d\.htaccess`, i))] {
			t.Errorf("dir%d missing from blacklist (%d of %d recorded)", i, len(set), n)
		}
	}
}

// Config files hold account metadata, so the atomic write has to land on 0600 —
// the temp file must never be committed with the mode os.CreateTemp or the
// umask would otherwise pick. Windows doesn't model these bits, so this can only
// assert on the platforms where it means something (Linux, and Android).
func TestSavesArePrivateToTheUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes don't carry POSIX permission bits")
	}
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	if err := d.SaveSettings(Settings{BaseDir: "/sync"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(d.SettingsFile())
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("settings.json mode = %o, want 600", got)
	}
}

// strayTempFiles lists anything in dir that looks like an uncommitted temp file.
func strayTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}
