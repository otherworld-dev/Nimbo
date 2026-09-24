package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/notify"
)

// Each account needs a folder of its own. GitHub #11: a second account was
// offered the first account's folder, and nothing stopped two accounts syncing
// the same folder against two servers, where each would upload the other's
// files.
//
// Two different questions are asked, against two different lists:
//
//   - Choosing a folder (setup, adding, moving, changing the account folder)
//     is checked against everything another account has CLAIMED: its account
//     folder, its sync folders and the ones parked while on-demand mode is on,
//     so a choice can't collide with a folder that will come back later.
//   - Syncing a folder is checked only against what another account is
//     actually SYNCING at the time: its live folders in live mode, its mounted
//     root in on-demand mode. A parked folder or a "choose"-mode parent syncs
//     nothing, and counting those let two accounts block each other for good.
//
// A folder that fails the second check is not moved or parked: it is gated in
// the engine (Engine.SetPairGate), stays in the account's setup, and resumes
// by itself once the other account has moved away.

// accountFolder is a folder in use by an account, labelled for messages.
type accountFolder struct {
	Account string // "bob on cloud.example.com"
	Dir     string
}

// accountLabel names an account the way the user will recognise it.
func accountLabel(acc account.Account) string {
	host := acc.ServerURL
	if u, err := url.Parse(acc.ServerURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return acc.LoginName + " on " + host
}

// folderList collects labelled folders without repeats.
type folderList struct {
	out  []accountFolder
	seen map[string]bool
}

func (l *folderList) add(label, dir string) {
	if dir == "" {
		return
	}
	dir = filepath.Clean(dir)
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	if l.seen[label+"\x00"+dir] {
		return
	}
	l.seen[label+"\x00"+dir] = true
	l.out = append(l.out, accountFolder{Account: label, Dir: dir})
}

// otherAccountFolders lists every folder CLAIMED by an account other than
// exclude: its account folder, its sync pairs, and the pairs parked while
// on-demand mode is on (they come back when it is switched off).
func otherAccountFolders(d config.Dirs, st account.Store, exclude string) []accountFolder {
	var l folderList
	for _, acc := range st.Accounts {
		if acc.ID == exclude {
			continue
		}
		ad := d.WithAccount(acc.ID)
		label := accountLabel(acc)
		if s, err := ad.LoadAccountState(); err == nil {
			l.add(label, s.BaseDir)
			l.add(label, s.OnDemandRoot)
			for _, p := range s.RememberedPairs {
				l.add(label, p.LocalDir)
			}
		}
		if pairs, err := ad.LoadPairs(); err == nil {
			for _, p := range pairs {
				l.add(label, p.LocalDir)
			}
		}
	}
	return l.out
}

// activeAccountFolders lists the folders accounts other than exclude are
// SYNCING in the given mode: their live pairs in live mode; in on-demand mode
// the root each mounts, in onDemandRootOrder.
func activeAccountFolders(d config.Dirs, st account.Store, exclude, mode string) []accountFolder {
	var l folderList
	for _, acc := range st.Accounts {
		if acc.ID == exclude {
			continue
		}
		ad := d.WithAccount(acc.ID)
		label := accountLabel(acc)
		pairs, _ := ad.LoadPairs()
		if mode != "ondemand" {
			for _, p := range pairs {
				l.add(label, p.LocalDir)
			}
			continue
		}
		as, _ := ad.LoadAccountState()
		pair := ""
		for _, p := range pairs {
			if strings.Trim(p.RemoteRoot, "/") == "" {
				pair = p.LocalDir
				break
			}
		}
		l.add(label, onDemandRootOrder(pair, as.OnDemandRoot, as.BaseDir))
	}
	return l.out
}

// folderClash returns a message if dir is, sits inside, or contains a folder
// in others; "" if it is free.
func folderClash(dir string, others []accountFolder) string {
	dir = filepath.Clean(dir)
	for _, o := range others {
		if pathWithin(dir, o.Dir) || pathWithin(o.Dir, dir) {
			return fmt.Sprintf("%s is already used by your %s account (%s). "+
				"Each account needs its own folder, so choose one outside it.", dir, o.Account, o.Dir)
		}
	}
	return ""
}

// suggestAttempts bounds the numbered candidates suggestAccountFolder tries.
// When another account's folder contains home, every candidate clashes, and an
// unbounded search hung start-up.
const suggestAttempts = 50

// suggestAccountFolder picks a default folder for an account: ~/Nextcloud when
// no other account uses it, otherwise "<brand> - <login>" in home, numbered if
// that is taken too. usable (nil = any) can reject a candidate, e.g. one that
// already holds files when the folder will be mounted without the user
// choosing it. Returns "" when nothing suitable is found.
func suggestAccountFolder(home, login string, others []accountFolder, usable func(string) bool) string {
	ok := func(dir string) bool {
		return folderClash(dir, others) == "" && (usable == nil || usable(dir))
	}
	if dir := filepath.Join(home, "Nextcloud"); ok(dir) {
		return dir
	}
	name := brand.Current.Name
	if login != "" {
		name += " - " + login
	}
	base := filepath.Join(home, name)
	if ok(base) {
		return base
	}
	for n := 2; n < suggestAttempts; n++ {
		if dir := fmt.Sprintf("%s (%d)", base, n); ok(dir) {
			return dir
		}
	}
	return ""
}

// missingOrEmpty accepts a folder that doesn't exist yet or holds nothing.
func missingOrEmpty(dir string) bool {
	fi, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && fi.IsDir() && folderEmpty(dir)
}

// accountsAndDirs loads the account store and config dirs, or reports false.
func accountsAndDirs() (config.Dirs, account.Store, bool) {
	d, err := config.Resolve()
	if err != nil {
		return config.Dirs{}, account.Store{}, false
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return config.Dirs{}, account.Store{}, false
	}
	return d, *st, true
}

// activeAccountID is the account whose folder is being set: the running
// engine's, or the default account's before an engine exists.
func (a *App) activeAccountID(st account.Store) string {
	if a.eng != nil {
		return a.eng.Account.ID
	}
	if acc, ok := st.Default(); ok {
		return acc.ID
	}
	return ""
}

// folderClashFor checks a folder the user is CHOOSING for the active account
// against everything the other accounts have claimed.
func (a *App) folderClashFor(dir string) string {
	d, st, ok := accountsAndDirs()
	if !ok {
		return ""
	}
	return folderClash(dir, otherAccountFolders(d, st, a.activeAccountID(st)))
}

// syncClashFor checks a folder an account is about to SYNC or mount against
// what the other accounts are syncing right now.
func (a *App) syncClashFor(accountID, dir string) string {
	d, st, ok := accountsAndDirs()
	if !ok {
		return ""
	}
	return folderClash(dir, activeAccountFolders(d, st, accountID, a.GetSyncMode()))
}

// prepareLivePairs readies an account's live sync pairs before its engine
// runs: pairs parked by an earlier on-demand spell come back, and a folder
// another account is syncing is gated rather than synced (the #11 state, where
// each account would upload the other's files). The gate is asked again
// whenever watchers start, so a held folder resumes once the other account has
// moved away. Returns the pairs to run.
func (a *App) prepareLivePairs(eng *agent.Engine) []config.SyncPair {
	id := eng.Account.ID
	eng.SetPairGate(func(dir string) string { return a.syncClashFor(id, dir) })
	eng.RestoreRememberedPairs()
	pairs, _ := eng.Pairs()
	var run []config.SyncPair
	for _, p := range pairs {
		if why := eng.HeldReason(p.LocalDir); why != "" {
			a.warnFolderClash(eng, p.LocalDir, why)
			continue
		}
		run = append(run, p)
	}
	return run
}

// warnFolderClash reports a folder that is not synced or mounted because
// another account uses it. It runs on every start while the clash lasts, so
// the user is not left with a folder that has silently stopped.
func (a *App) warnFolderClash(eng *agent.Engine, dir, why string) {
	slog.Warn("not syncing a folder another account uses", "account", eng.Account.LoginName, "dir", dir, "why", why)
	a.toastAccount(fmt.Sprintf(
		"Not syncing %s for %s: another account uses the same folder. Choose a different folder for one of them in Settings.",
		dir, accountLabel(eng.Account)))
}

// warnNoFolder reports an account that could not be given a folder to mount
// because no free, empty one was found.
func (a *App) warnNoFolder(eng *agent.Engine) {
	slog.Warn("no free, empty folder to mount for an account", "account", eng.Account.LoginName)
	a.toastAccount(fmt.Sprintf("%s has no folder yet. Choose a folder for it in Settings.", accountLabel(eng.Account)))
}

func (a *App) toastAccount(msg string) {
	if a.NotificationsEnabled() {
		notify.Toast(brand.Current.Name, msg, "")
	}
}

// suggestedFolder is the default folder to show the active account when it
// has none of its own yet. It never returns "", so the UI always has a path to
// show; if nothing is free it falls back to ~/Nextcloud, which every place that
// actually uses a folder checks before doing so.
func (a *App) suggestedFolder() string {
	home, _ := os.UserHomeDir()
	login := ""
	if a.eng != nil {
		login = a.eng.Account.LoginName
	}
	d, st, ok := accountsAndDirs()
	var others []accountFolder
	var usable func(string) bool
	if ok {
		others = otherAccountFolders(d, st, a.activeAccountID(st))
		// With other accounts on this PC, a folder that already holds files is
		// most likely one of theirs (or left over from one), and keeping its
		// files would upload them to this account's server. Only the first
		// account is offered a folder with files in it, e.g. the official
		// client's old folder, which the setup screen then offers to adopt.
		if len(st.Accounts) > 1 {
			usable = missingOrEmpty
		}
	}
	if dir := suggestAccountFolder(home, login, others, usable); dir != "" {
		return dir
	}
	return filepath.Join(home, "Nextcloud")
}

// heldReasoner is the part of the engine localDeleteBlocked needs.
type heldReasoner interface{ HeldReason(localDir string) string }

// localDeleteBlocked refuses deleting the local files of a held-back folder:
// another account syncs the same folder, so the files are that account's too,
// and it would push the deletions to its server once it resumes. Removing or
// deselecting the folder while keeping the files is always allowed.
func localDeleteBlocked(eng heldReasoner, localDir string, deleteLocal bool) string {
	if !deleteLocal || eng.HeldReason(localDir) == "" {
		return ""
	}
	return "Another account uses this folder too, so its files can't be deleted from here. " +
		"Remove it and keep the files, or deal with it from the other account."
}


// regateAll asks every engine's gate again, so a folder freed by a change in
// one account (removed, moved, re-pointed) starts syncing in another at once
// rather than at its next start. Live mode only: in on-demand mode an
// account's pairs must never get live watchers.
func (a *App) regateAll() {
	if a.GetSyncMode() == "ondemand" {
		return
	}
	if a.eng != nil {
		_ = a.eng.ReloadPairs()
	}
	for _, se := range a.secondaries {
		_ = se.eng.ReloadPairs()
	}
}

// abandonedRoots picks, from the roots mounted before a virtual-files restart,
// the ones nothing mounts now and no account claims: typically the folder a
// new account was given before its setup chose a different one. Only those are
// unregistered; every other registration is kept, because unregistering strips
// the cloud state from the whole tree.
func abandonedRoots(before []string, mountedNow map[string]bool, claimed []string) []string {
	var out []string
	for _, dir := range before {
		// Keeping a registration is always the safe direction, so a root that
		// holds, or sits in, a claimed folder is kept too.
		if mountedNow[dir] || overlapsAny(dir, claimed) {
			continue
		}
		out = append(out, dir)
	}
	return out
}


// mountedRoots lists the folders mounted right now, with the owning account.
func (a *App) mountedRoots() map[string]string {
	out := make(map[string]string, len(a.onDemandMounts))
	for dir, m := range a.onDemandMounts {
		out[dir] = m.accountID
	}
	return out
}

// unregisterAbandonedRoots unregisters the roots mounted before a restart
// that nothing mounts or claims now (see abandonedRoots).
func (a *App) unregisterAbandonedRoots(before map[string]string) {
	if a.eng == nil {
		return
	}
	d, st, ok := accountsAndDirs()
	if !ok {
		return
	}
	claimed, ok := claimedFoldersStrict(d, st)
	if !ok {
		return // never unregister on a guess
	}
	var dirs []string
	for dir, owner := range before {
		// Only the shown account's own roots: another account's registration
		// is never this restart's to drop.
		if owner == a.eng.Account.ID {
			dirs = append(dirs, dir)
		}
	}
	now := map[string]bool{}
	for dir := range a.onDemandMounts {
		now[dir] = true
	}
	for _, dir := range abandonedRoots(dirs, now, claimed) {
		slog.Info("unregistering a virtual-files root no account uses any more", "dir", dir)
		cfapi.UnregisterShellSyncRoot(dir)
		_ = cfapi.UnregisterSyncRoot(dir)
	}
}

// unmountOnDemandFor permanently unmounts ONE account's virtual-files roots
// (sign-out, removal): the registration goes and Windows strips the cloud
// state from those trees, by design. Other accounts' mounts are left alone;
// tearing them down too flattened the accounts that stayed (GitHub #11).
func (a *App) unmountOnDemandFor(accountID string) {
	for dir, m := range a.onDemandMounts {
		if m.accountID != accountID {
			continue
		}
		if m.convertCancel != nil {
			m.convertCancel()
		}
		if m.watcher != nil {
			m.watcher.Close()
		}
		if m.healCancel != nil {
			m.healCancel()
		}
		cfapi.Unmount(dir, m.connKey)
		clearAdoptResume(m.accountID)
		delete(a.onDemandMounts, dir)
		slog.Info("on-demand mount removed", "dir", dir, "account", accountID)
	}
	a.refreshOverlayRoots()
}

// shownAccountMounted reports whether the shown account has a virtual-files
// root mounted, whichever folder it is.
func (a *App) shownAccountMounted() bool {
	if a.eng == nil {
		return false
	}
	for _, m := range a.onDemandMounts {
		if m.accountID == a.eng.Account.ID {
			return true
		}
	}
	return false
}

// forgetOnDemandRoot drops an account's recorded virtual-files root (the
// account left virtual files, so the folder is no longer one).
func forgetOnDemandRoot(accountID string) {
	if d, err := config.Resolve(); err == nil {
		_ = d.WithAccount(accountID).UpdateAccountState(func(s *config.AccountState) { s.OnDemandRoot = "" })
	}
}
