// Package config resolves the per-OS locations Nimbo uses to store its
// configuration, account metadata, and local sync state. Secrets (app
// passwords) are never written here; they live in the OS keychain (see the
// account package).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/otherworld/nimbo/internal/brand"
)

// legacyAppDir is the pre-rename directory; the stock build migrates it to the
// current appDir so an existing install's account/settings/state carry over.
const legacyAppDir = "nextclient"

// appDir is the subdirectory name used under the OS config/data roots. It is
// derived from the active brand's app id so a white-label build keeps its
// config/data in its OWN folder (e.g. %AppData%\acmecloud) instead of sharing
// stock Nimbo's. The stock brand's appId "Nimbo" slugs to "nimbo", so existing
// installs are unaffected. Brand is embedded at build time, so this is fixed per
// build.
var appDir = brandSlug(brand.Current.AppID)

// brandSlug turns a brand identifier into a filesystem-safe folder name:
// lowercased, ASCII letters/digits kept, every other run collapsed to a single
// hyphen, then trimmed. Falls back to "nimbo" if nothing usable remains.
func brandSlug(s string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			hyphen = false
		} else if b.Len() > 0 && !hyphen {
			b.WriteByte('-')
			hyphen = true
		}
	}
	if out := strings.Trim(b.String(), "-"); out != "" {
		return out
	}
	return "nimbo"
}

// activeAppDir returns the directory name this process should use: the
// brand's appDir for the packaged app (and every non-Windows build), and
// appDir+"-dev" for a Windows process running WITHOUT MSIX package identity —
// dev builds and the bare CLI. Unpackaged processes must not share the
// packaged app's directories: Windows virtualizes %AppData%/%LocalAppData%
// for the packaged app and can re-seed that virtualized copy FROM the real
// directories during a package repair. A dev-run state DB left in the real
// directory became the packaged app's sync state that way (2026-07-24): the
// app read it as "never synced" and silently started a takeover clone.
func activeAppDir() string {
	if hasPackageIdentity() {
		return appDir
	}
	return appDir + "-dev"
}

// Dirs holds the resolved on-disk locations for this machine/user.
type Dirs struct {
	// Config holds user configuration and account metadata (accounts.json).
	Config string
	// Data holds local sync state such as the SQLite baseline database.
	Data string
	// acct scopes per-account files (sync pairs, on-demand etags) when set via
	// WithAccount. Empty means the legacy unscoped paths (single-account era).
	acct string
}

// WithAccount returns a copy of d whose per-account files (PairsFile,
// VFSETagsFile) are scoped to the given account ID. Call MigratePairs once
// after binding so a pre-multi-account install keeps its sync setup.
func (d Dirs) WithAccount(id string) Dirs {
	d.acct = id
	return d
}

// AccountID returns the account this Dirs is scoped to ("" if unscoped).
func (d Dirs) AccountID() string { return d.acct }

// Resolve determines the config and data directories for the current user,
// creating them if they do not yet exist. It honours the platform conventions
// provided by os.UserConfigDir / os.UserCacheDir, e.g. on Windows both fall
// under %AppData%/%LocalAppData%, on Linux under ~/.config and ~/.local/share.
func Resolve() (Dirs, error) {
	cfgRoot, err := os.UserConfigDir()
	if err != nil {
		return Dirs{}, fmt.Errorf("locate user config dir: %w", err)
	}
	// Prefer a dedicated data dir; fall back to the config root if the OS does
	// not distinguish one (older Go on some platforms).
	dataRoot, err := userDataDir()
	if err != nil {
		dataRoot = cfgRoot
	}

	dir := activeAppDir()
	migrateLegacyDir(cfgRoot, dir)
	migrateLegacyDir(dataRoot, dir)

	d := Dirs{
		Config: filepath.Join(cfgRoot, dir),
		Data:   filepath.Join(dataRoot, dir),
	}
	for _, p := range []string{d.Config, d.Data} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return Dirs{}, fmt.Errorf("create dir %s: %w", p, err)
		}
	}
	return d, nil
}

// migrateLegacyDir renames root/<legacyAppDir> to root/<dir> when the new
// one doesn't yet exist, so a pre-rename install keeps its data. This applies
// only to the stock PACKAGED build: the legacy "nextclient" data belonged to
// stock Nimbo, so a white-label build (a distinct app) must never adopt it —
// and neither may a dev/CLI process (dir "nimbo-dev"), which starts fresh.
func migrateLegacyDir(root, dir string) {
	newp := filepath.Join(root, dir)
	if _, err := os.Stat(newp); err == nil {
		return // already migrated / fresh install
	}
	if dir != "nimbo" {
		return // white-label or dev dir: do not inherit stock's legacy directory
	}
	oldp := filepath.Join(root, legacyAppDir)
	if fi, err := os.Stat(oldp); err == nil && fi.IsDir() {
		_ = os.Rename(oldp, newp)
	}
}

// AccountsFile is the path to the account metadata store.
func (d Dirs) AccountsFile() string {
	return filepath.Join(d.Config, "accounts.json")
}

// StateDB is the path to an account's local sync-state database.
func (d Dirs) StateDB(accountID string) string {
	return filepath.Join(d.Data, "state-"+accountID+".db")
}

// LicenceFile is the path to the installed business licence token (org-wide,
// account-independent).
func (d Dirs) LicenceFile() string {
	return filepath.Join(d.Config, "license.key")
}

// LogFile is the path to the rotating application log.
func (d Dirs) LogFile() string {
	return filepath.Join(d.Data, "logs", "nimbo.log")
}
