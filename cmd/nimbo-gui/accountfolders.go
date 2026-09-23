package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/notify"
)

// Each account needs a folder of its own. GitHub #11: a second account was
// offered the first account's folder, and nothing stopped two accounts syncing
// the same folder against two servers, where each would upload the other's
// files. Every place that sets an account's folder checks it against the
// folders of all the other accounts first.

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

// otherAccountFolders lists every folder used by an account other than
// exclude: its account folder, its sync pairs, and the pairs parked while
// on-demand mode is on (they come back when it is switched off).
func otherAccountFolders(d config.Dirs, st account.Store, exclude string) []accountFolder {
	var out []accountFolder
	seen := map[string]bool{}
	add := func(label, dir string) {
		if dir == "" {
			return
		}
		dir = filepath.Clean(dir)
		if seen[label+"\x00"+dir] {
			return
		}
		seen[label+"\x00"+dir] = true
		out = append(out, accountFolder{Account: label, Dir: dir})
	}
	for _, acc := range st.Accounts {
		if acc.ID == exclude {
			continue
		}
		ad := d.WithAccount(acc.ID)
		label := accountLabel(acc)
		if s, err := ad.LoadAccountState(); err == nil {
			add(label, s.BaseDir)
			for _, p := range s.RememberedPairs {
				add(label, p.LocalDir)
			}
		}
		if pairs, err := ad.LoadPairs(); err == nil {
			for _, p := range pairs {
				add(label, p.LocalDir)
			}
		}
	}
	return out
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

// suggestAccountFolder picks a default folder for an account: ~/Nextcloud when
// no other account uses it, otherwise "<brand> - <login>" in the home folder,
// numbered if that is taken too.
func suggestAccountFolder(home, login string, others []accountFolder) string {
	free := func(dir string) bool { return folderClash(dir, others) == "" }
	if dir := filepath.Join(home, "Nextcloud"); free(dir) {
		return dir
	}
	base := filepath.Join(home, brand.Current.Name+" - "+login)
	if free(base) {
		return base
	}
	for n := 2; ; n++ {
		if dir := fmt.Sprintf("%s (%d)", base, n); free(dir) {
			return dir
		}
	}
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

// otherFolders lists the folders used by every account except the active one.
func (a *App) otherFolders() []accountFolder {
	d, err := config.Resolve()
	if err != nil {
		return nil
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return nil
	}
	return otherAccountFolders(d, *st, a.activeAccountID(*st))
}

// folderClashFor checks dir against every other account's folders.
func (a *App) folderClashFor(dir string) string {
	return folderClash(dir, a.otherFolders())
}

// foldersOtherThan lists the folders used by every account except accountID.
func foldersOtherThan(accountID string) []accountFolder {
	d, err := config.Resolve()
	if err != nil {
		return nil
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return nil
	}
	return otherAccountFolders(d, *st, accountID)
}

// prepareLivePairs readies an account's live sync pairs before its engine
// runs. A pair that overlaps another account's folder is parked rather than
// synced: an install already in the #11 state has two accounts on one folder,
// and each would upload the other's files. Parked pairs that no longer overlap
// come back. The user is told once per start what was held back and why.
func (a *App) prepareLivePairs(eng *agent.Engine) []config.SyncPair {
	others := foldersOtherThan(eng.Account.ID)
	held := eng.HoldBackPairs(func(dir string) bool { return folderClash(dir, others) != "" })
	eng.RestoreRememberedPairs(func(dir string) error {
		if msg := folderClash(dir, others); msg != "" {
			return errors.New(msg)
		}
		return nil
	})
	if len(held) > 0 {
		a.warnFolderClash(eng, held[0].LocalDir, folderClash(held[0].LocalDir, others))
	}
	pairs, _ := eng.Pairs()
	return pairs
}

// warnFolderClash reports a folder that was not synced or mounted because
// another account uses it: in the log always, and as a toast.
func (a *App) warnFolderClash(eng *agent.Engine, dir, why string) {
	slog.Warn("not syncing a folder another account uses", "account", eng.Account.LoginName, "dir", dir, "why", why)
	if a.NotificationsEnabled() {
		notify.Toast(brand.Current.Name, fmt.Sprintf(
			"Not syncing %s for %s: another account uses the same folder. Choose a different folder for one of them in Settings.",
			dir, accountLabel(eng.Account)), "")
	}
}

// suggestedFolder is the default folder to offer the active account when it
// has none of its own yet.
func (a *App) suggestedFolder() string {
	home, _ := os.UserHomeDir()
	login := ""
	if a.eng != nil {
		login = a.eng.Account.LoginName
	}
	return suggestAccountFolder(home, login, a.otherFolders())
}
