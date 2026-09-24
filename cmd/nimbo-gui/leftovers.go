package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/shellns"
)

// Leftover sync roots and sidebar entries (GitHub #10). Older versions left
// virtual-files roots registered when an account was switched, removed or
// moved to another folder, and each one keeps an Explorer sidebar entry
// pointing at a folder nothing syncs. Windows has also been seen to leave the
// entries behind after the roots themselves were gone. These are cleaned up
// once per launch, conservatively: unregistering a root strips the cloud state
// from its folder, so it only happens when every account's setup could be read
// and none of them uses the folder.

// claimedFoldersStrict lists every folder any configured account uses (its
// account folder, virtual-files root, sync pairs and parked pairs), or reports
// false when any of it can't be read: a destructive clean-up must never treat
// "couldn't read" as "not used".
func claimedFoldersStrict(d config.Dirs, st account.Store) ([]string, bool) {
	var out []string
	for _, acc := range st.Accounts {
		ad := d.WithAccount(acc.ID)
		s, err := ad.LoadAccountState()
		if err != nil {
			return nil, false
		}
		pairs, err := ad.LoadPairs()
		if err != nil {
			return nil, false
		}
		for _, dir := range []string{s.BaseDir, s.OnDemandRoot} {
			if dir != "" {
				out = append(out, dir)
			}
		}
		for _, p := range append(pairs, s.RememberedPairs...) {
			out = append(out, p.LocalDir)
		}
	}
	return out, true
}

// overlapsAny reports whether dir is, contains, or sits inside any of dirs.
func overlapsAny(dir string, dirs []string) bool {
	for _, c := range dirs {
		if pathWithin(dir, c) || pathWithin(c, dir) {
			return true
		}
	}
	return false
}

// leftoverRoots picks the registered roots that nothing mounts now and whose
// folder overlaps nothing any account uses.
func leftoverRoots(roots []cfapi.ShellSyncRoot, claimed []string, mounted map[string]bool) []cfapi.ShellSyncRoot {
	var out []cfapi.ShellSyncRoot
	for _, r := range roots {
		if r.Path == "" || mounted[filepath.Clean(r.Path)] || overlapsAny(r.Path, claimed) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// orphanNavNodes picks the Nimbo sidebar entries that belong to no registered
// sync root (live holds the lower-cased NamespaceCLSIDs of the roots).
func orphanNavNodes(nodes []shellns.NavNode, live map[string]bool) []shellns.NavNode {
	var out []shellns.NavNode
	for _, n := range nodes {
		if !live[strings.ToLower(n.CLSID)] {
			out = append(out, n)
		}
	}
	return out
}

// rootOwnedBy reports whether a root, identified by the icon path it was
// registered with ("<exe>,0"), was registered by the same kind of build as
// exe. The installed app owns roots from any version of its own package
// (same name and publisher); a development build owns roots registered by its
// own executable. Each has only its own account configuration, so it must not
// judge, let alone unregister, the other's roots.
func rootOwnedBy(icon, exe string) bool {
	if i := strings.LastIndex(icon, ","); i >= 0 {
		icon = icon[:i]
	}
	if icon == "" || exe == "" {
		return false
	}
	iconDir, exeDir := filepath.Dir(icon), filepath.Dir(exe)
	if strings.EqualFold(iconDir, exeDir) {
		return true
	}
	name, pub, ok := packageParts(filepath.Base(exeDir))
	if !ok {
		return false
	}
	iname, ipub, ok := packageParts(filepath.Base(iconDir))
	return ok && strings.EqualFold(name, iname) && strings.EqualFold(pub, ipub) &&
		strings.EqualFold(filepath.Dir(iconDir), filepath.Dir(exeDir))
}

// packageParts splits an MSIX install folder name "<name>_<ver>_<arch>__<pub>"
// into its name and publisher id.
func packageParts(dir string) (name, pub string, ok bool) {
	i := strings.Index(dir, "_")
	j := strings.LastIndex(dir, "__")
	if i <= 0 || j <= i {
		return "", "", false
	}
	return dir[:i], dir[j+2:], true
}

// missingAction is what to do about an account's virtual-files root.
type missingAction int

const (
	mountRoot    missingAction = iota // there, or never registered: mount it
	waitForRoot                       // gone while registered: wait for it to come back
	recreateRoot                      // gone for a day: drop the registration, set up again
)

// missingRootWait is how long a vanished root is waited for before it is set
// up again from scratch.
const missingRootWait = 24 * time.Hour

// missingRootAction decides about a root folder that may have vanished. A
// folder that is gone while still registered was most likely renamed or moved
// (the reporter renamed his). Re-creating it at once, as mounting used to,
// leaves the moved copy's files stranded for good; renaming it back while it
// is still registered is what recovers it. So Nimbo waits for it, and only
// after a day drops the registration and sets the folder up again. since is
// when it was first seen missing (zero = not missing); the returned value is
// what to store.
func missingRootAction(exists, registered bool, since, now time.Time) (missingAction, time.Time) {
	switch {
	case exists || !registered:
		return mountRoot, time.Time{}
	case since.IsZero():
		return waitForRoot, now
	case now.Sub(since) < missingRootWait:
		return waitForRoot, since
	default:
		return recreateRoot, time.Time{}
	}
}

// unchosenUsable accepts a folder for mounting without the user choosing it:
// missing or empty; or already a registered root when this is the only
// account (a single-account install keeps its folder) or the folder is this
// account's own ownDir. With several accounts, another registered root may
// hold another account's files.
func unchosenUsable(registered func(string) bool, single bool, ownDir string) func(string) bool {
	return func(dir string) bool {
		if missingOrEmpty(dir) {
			return true
		}
		if !registered(dir) {
			return false
		}
		return single || (ownDir != "" && strings.EqualFold(filepath.Clean(dir), filepath.Clean(ownDir)))
	}
}

// onDemandRootOrder is the one order an account's virtual-files root is
// decided in, whether it is the shown account or a background one: its
// whole-account pair, then the root it last mounted, then its account folder.
func onDemandRootOrder(wholePair, recorded, baseDir string) string {
	for _, d := range []string{wholePair, recorded, baseDir} {
		if d != "" {
			return d
		}
	}
	return ""
}

// wholeAccountPair returns the account's whole-account pair folder, or "".
func wholeAccountPair(eng *agent.Engine) string {
	pairs, err := eng.Pairs()
	if err != nil {
		return ""
	}
	for _, p := range pairs {
		if strings.Trim(p.RemoteRoot, "/") == "" {
			return p.LocalDir
		}
	}
	return ""
}

// accountCount is the number of configured accounts (0 if unreadable).
func accountCount() int {
	_, st, ok := accountsAndDirs()
	if !ok {
		return 0
	}
	return len(st.Accounts)
}

// ownFolderName is the account's own default folder name, "<brand> - <login>".
func ownFolderName(home, login string) string {
	return filepath.Join(home, brand.Current.Name+" - "+login)
}

// rootReady applies missingRootAction to an account's root before it is
// mounted, persisting when the folder was first seen missing. It returns false
// when the mount must wait for the folder to come back.
func (a *App) rootReady(eng *agent.Engine, dir string) bool {
	d, err := config.Resolve()
	if err != nil {
		return true
	}
	ad := d.WithAccount(eng.Account.ID)
	s, _ := ad.LoadAccountState()
	since, _ := time.Parse(time.RFC3339, s.RootMissingSince)
	_, statErr := os.Stat(dir)
	act, next := missingRootAction(statErr == nil, cfapi.ShellSyncRootRegistered(dir), since, time.Now())
	stamp := ""
	if !next.IsZero() {
		stamp = next.UTC().Format(time.RFC3339)
	}
	if stamp != s.RootMissingSince {
		_ = ad.UpdateAccountState(func(s *config.AccountState) { s.RootMissingSince = stamp })
	}
	switch act {
	case waitForRoot:
		msg := fmt.Sprintf("%s is missing. If you renamed or moved it, move it back to %s and %s will reconnect it. "+
			"Otherwise it will be set up again in a day.", filepath.Base(dir), dir, brand.Current.Name)
		if eng == a.eng {
			a.mountRefused = msg
		}
		slog.Warn("virtual-files root is missing but still registered; waiting for it", "account", eng.Account.LoginName, "dir", dir)
		a.toastAccount(msg)
		return false
	case recreateRoot:
		slog.Warn("virtual-files root missing for a day; setting it up again", "account", eng.Account.LoginName, "dir", dir)
		cfapi.UnregisterShellSyncRoot(dir)
		_ = cfapi.UnregisterSyncRoot(dir)
	}
	return true
}

// unregisterRecordedRoot unregisters an account's recorded virtual-files root
// when it signs out or is removed while not mounted (its engine wasn't
// running), so its sidebar entry doesn't outlive it. Only when every account's
// setup can be read and no other account uses the folder.
func (a *App) unregisterRecordedRoot(accountID string) {
	d, st, ok := accountsAndDirs()
	if !ok {
		return
	}
	ad := d.WithAccount(accountID)
	s, err := ad.LoadAccountState()
	if err != nil {
		return
	}
	var others account.Store
	for _, acc := range st.Accounts {
		if acc.ID != accountID {
			others.Accounts = append(others.Accounts, acc)
		}
	}
	claimed, ok := claimedFoldersStrict(d, others)
	if !ok {
		return
	}
	cands := []string{s.OnDemandRoot}
	if pairs, err := ad.LoadPairs(); err == nil {
		for _, p := range pairs {
			if strings.Trim(p.RemoteRoot, "/") == "" {
				cands = append(cands, p.LocalDir)
			}
		}
	}
	for _, dir := range cands {
		if dir == "" || a.onDemandMounts[dir] != nil || overlapsAny(dir, claimed) || !cfapi.ShellSyncRootRegistered(dir) {
			continue
		}
		slog.Info("unregistering the root of an account that is going", "account", accountID, "dir", dir)
		cfapi.UnregisterShellSyncRoot(dir)
		_ = cfapi.UnregisterSyncRoot(dir)
	}
}

// sweepLeftovers unregisters leftover roots and removes orphaned sidebar
// entries. Run once per launch, after every account has mounted; mounted is a
// snapshot of the folders mounted then.
func (a *App) sweepLeftovers(mounted map[string]bool) {
	if !cfapi.Supported() {
		return
	}
	d, st, ok := accountsAndDirs()
	if !ok {
		return
	}
	claimed, ok := claimedFoldersStrict(d, st)
	if !ok {
		slog.Warn("leftover clean-up skipped: an account's folder setup couldn't be read")
		return
	}
	exe, _ := os.Executable()
	for _, r := range leftoverRoots(cfapi.NimboShellSyncRoots(), claimed, mounted) {
		if !rootOwnedBy(r.Icon, exe) {
			continue
		}
		slog.Info("unregistering a leftover sync root no account uses", "dir", r.Path, "id", r.ID)
		cfapi.UnregisterShellSyncRootByID(r.ID, r.Path)
	}
	live := map[string]bool{}
	for _, r := range cfapi.NimboShellSyncRoots() {
		if r.NamespaceCLSID != "" {
			live[strings.ToLower(r.NamespaceCLSID)] = true
		}
	}
	var clsids []string
	for _, n := range orphanNavNodes(shellns.NimboNavNodes(), live) {
		slog.Info("removing a leftover sidebar entry", "clsid", n.CLSID, "target", n.Target)
		clsids = append(clsids, n.CLSID)
	}
	if err := shellns.RemoveNavNodes(clsids); err != nil {
		slog.Warn("could not remove leftover sidebar entries", "err", err)
	}
}
