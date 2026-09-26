package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Settings holds global client preferences.
type Settings struct {
	// Bandwidth caps in KiB/s; 0 means unlimited.
	UploadKBps   int `json:"uploadKBps"`
	DownloadKBps int `json:"downloadKBps"`
	// BaseDir is LEGACY: the account folder is per account now
	// (AccountState.BaseDir). Kept only so MigrateAccountFolder can move an
	// older install's value to the account it belonged to; nothing else reads it.
	BaseDir string `json:"baseDir"`
	// PinnedApps holds the IDs of Nextcloud apps pinned to the flyout.
	PinnedApps []string `json:"pinnedApps"`
	// HideAppDock hides the flyout's app dock (the rail of pinned apps) when true.
	// Zero value (false) keeps the dock visible.
	HideAppDock bool `json:"hideAppDock"`
	// AppDockSide is which edge the app dock sits on: "right" (default) or "left".
	AppDockSide string `json:"appDockSide"`
	// HideSearch hides the flyout's file-search bar when true. Zero value (false)
	// keeps it visible.
	HideSearch bool `json:"hideSearch"`
	// Flyout appearance customization (all optional; zero values = the defaults).
	// DockIconSize sizes the pinned-app icons: "small" | "medium" (default) | "large".
	DockIconSize string `json:"dockIconSize"`
	// PanelWidth sizes the flyout window: "compact" | "standard" (default) | "wide".
	PanelWidth string `json:"panelWidth"`
	// Density controls spacing/font size: "comfortable" (default) | "compact".
	Density string `json:"density"`
	// FlyoutSections is the ordered set of visible middle sections (a subset of
	// "search", "activity", "storage"); nil = the default order. A section absent
	// from the list is hidden.
	FlyoutSections []string `json:"flyoutSections"`
	// MuteNotifications disables desktop toasts when true (default: notifications on).
	MuteNotifications bool `json:"muteNotifications"`
	// VerboseLog enables debug-level logging when true.
	VerboseLog bool `json:"verboseLog"`
	// BetaUpdates opts this installation into pre-release builds: update checks
	// then offer GitHub pre-releases as well as stable releases. Default (false)
	// is the stable channel. Per installation, not per account — and inert on
	// builds that can't self-update (the Store build updates itself).
	BetaUpdates bool `json:"betaUpdates"`
	// KeepBaselineInMemory holds the whole sync baseline resident in RAM for faster
	// warm syncs. Default (false) is "low memory mode": read it from disk per sync,
	// keeping the footprint small (the UI presents the inverse as "Low memory mode").
	KeepBaselineInMemory bool `json:"keepBaselineInMemory"`
	// Quiet-hours auto-pause window (minutes from midnight; end < start wraps).
	PauseScheduleEnabled bool `json:"pauseScheduleEnabled"`
	PauseFromMin         int  `json:"pauseFromMin"`
	PauseToMin           int  `json:"pauseToMin"`
	// ConflictPolicy: "ask" (default), "keepboth", or "newest".
	ConflictPolicy string `json:"conflictPolicy"`
	// Theme: "system" (default), "light", or "dark".
	Theme string `json:"theme"`
	// SyncMode: "live" (default; real files, full two-way sync) or "ondemand"
	// (experimental; online-only placeholders that download when opened).
	SyncMode string `json:"syncMode"`
	// AllowedFilenames are exact filenames the user permits even when normally
	// blocked (the builtin web-config block — .htaccess/.htpasswd/.user.ini — or
	// the server's forbidden list). Use only if your server actually accepts them.
	AllowedFilenames []string `json:"allowedFilenames"`
	// EscapeExtensions are file extensions (e.g. ".htaccess") the user opted to sync
	// despite the server forbidding the name: a forbidden file of one of these types
	// is stored on the server with a marker suffix and decoded back locally (see the
	// Escaper). Empty (default) = no escaping. The admin policy layer can force this off.
	EscapeExtensions []string `json:"escapeExtensions"`
	// OnDemandMounts records on-demand (virtual-files) mount folders and the
	// remote path each mirrors, so they can be reconnected on the next launch
	// (and any sync root left registered after a hard exit can be healed).
	OnDemandMounts []OnDemandMount `json:"onDemandMounts"`
	// RememberedPairs is LEGACY, like BaseDir: the parked live pairs are per
	// account now (AccountState.RememberedPairs). Kept for MigrateAccountFolder.
	RememberedPairs []SyncPair `json:"rememberedPairs,omitempty"`
	// DevIgnoresSeeded records that dependency/VCS dir patterns (node_modules,
	// .git, …) were seeded into the user-editable global ignore list, so the
	// one-time migration never re-adds a pattern the user deliberately removed.
	// FileLocking: take a files_lock lock while a document is open, so colleagues
	// are told it is in use. OFF by default (zero value): a lock is visible to
	// other people and, on a server with no lock_timeout configured, a bug here
	// would strand a file that only an administrator could free.
	FileLocking bool `json:"fileLocking"`

	// FileLockout: also HOLD a file open locally while somebody else has it
	// locked, so the local editor refuses to edit it and shows its own "locked
	// by" dialog. OFF by default and gated separately from FileLocking: this is
	// the intrusive half, and a bug in the release path stops a user editing
	// their own document until Nimbo restarts.
	FileLockout bool `json:"fileLockout"`

	// BadgesOffered: the one-time "enable folder badges?" toast has been shown
	// (live mode, badges unregistered). The Settings button remains regardless.
	BadgesOffered bool `json:"badgesOffered"`

	DevIgnoresSeeded bool `json:"devIgnoresSeeded"`
	// AppWindowSizes remembers the last window size per Nextcloud-app window
	// (apps opened as desktop windows from the flyout dock), keyed by app id.
	AppWindowSizes map[string]AppWindowSize `json:"appWindowSizes,omitempty"`
	// AppShortcuts records created Start-menu shortcuts: app id → .lnk filename.
	// Keyed by id (not display name) so a server-side rename can't orphan the
	// shortcut — removal resolves the recorded filename.
	AppShortcuts map[string]string `json:"appShortcuts,omitempty"`
	// AppShortcutsOptOut marks apps whose Start-menu shortcut the user removed;
	// opening the app's window won't auto-create it again.
	AppShortcutsOptOut map[string]bool `json:"appShortcutsOptOut,omitempty"`
	// SidebarEnabled is whether the user wants the Explorer navigation-pane
	// entry. A pointer because "never chosen" must be distinguishable from
	// "off": on the first run of a build that applies the entry out of the MSIX
	// container we fall back to reading the registry, which is the only chance
	// to notice an entry an older (unpackaged) build left behind.
	SidebarEnabled *bool `json:"sidebarEnabled,omitempty"`
	// SidebarTarget is the folder the entry was last pointed at. A packaged
	// build cannot read this back — its own HKCU reads are answered from the
	// package's private hive, not the keys Explorer reads — so it has to
	// remember what it asked for to know when a rewrite is due.
	SidebarTarget string `json:"sidebarTarget,omitempty"`
}

// AppWindowSize is a remembered app-window size in logical pixels.
type AppWindowSize struct {
	W int `json:"w"`
	H int `json:"h"`
}

// OnDemandMount is one persisted virtual-files mount.
type OnDemandMount struct {
	Local  string `json:"local"`  // local sync-root folder
	Remote string `json:"remote"` // files-root-relative remote path ("" = account root)
}

// UnmarshalJSON accepts both the current object form and the legacy bare-string
// form (just a local path, account root), so older settings files migrate.
func (m *OnDemandMount) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		m.Local, m.Remote = s, ""
		return nil
	}
	type alias OnDemandMount
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*m = OnDemandMount(a)
	return nil
}

// SettingsFile is the path to the settings file.
func (d Dirs) SettingsFile() string {
	return filepath.Join(d.Config, "settings.json")
}

// LoadSettings reads settings; a missing file yields zero values (unlimited).
func (d Dirs) LoadSettings() (Settings, error) {
	var s Settings
	data, err := os.ReadFile(d.SettingsFile())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	return s, nil
}

// UpdateSettings applies mutate to the current settings and persists the result,
// holding the settings file's lock across the whole read-modify-write.
//
// PREFER THIS OVER LoadSettings + SaveSettings for changing one setting. Settings
// are written from the GUI's handler goroutines and from the engine's, and a bare
// load-mutate-save loses an update whenever two overlap: both read the same file,
// so the second one writes back the stale value it read for every field but its
// own. That is why an unlucky SetBaseDir could appear to do nothing.
//
// mutate runs under the lock, so it must not call back into UpdateSettings.
func (d Dirs) UpdateSettings(mutate func(*Settings)) error {
	mu := fileMutex(d.SettingsFile())
	mu.Lock()
	defer mu.Unlock()

	s, err := d.LoadSettings()
	if err != nil {
		return err
	}
	mutate(&s)
	return d.saveSettingsLocked(s)
}

// SaveSettings persists settings wholesale. Changing a single field is a job for
// UpdateSettings — this overwrites every other field with whatever s carries.
func (d Dirs) SaveSettings(s Settings) error {
	mu := fileMutex(d.SettingsFile())
	mu.Lock()
	defer mu.Unlock()
	return d.saveSettingsLocked(s)
}

func (d Dirs) saveSettingsLocked(s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileLocked(d.SettingsFile(), data, 0o600)
}
