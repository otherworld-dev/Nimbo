package main

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/config"
)

// sidebarLabel is the name an account's Explorer sidebar entry is shown under:
// the name the user chose for it (acc.Label) when there is one, otherwise the
// automatic one from autoSidebarLabel.
func sidebarLabel(acc account.Account, all []account.Account) string {
	if acc.Label != "" {
		return acc.Label
	}
	return autoSidebarLabel(acc, all)
}

// autoSidebarLabel is the product name and the login, "Nimbo - adam", so that
// with several accounts each entry says whose it is (GitHub #10, #19). Two
// accounts with the same login on different servers would still look alike,
// so then the server is added as well: "Nimbo - adam on cloud.example.com".
//
// The login is shown even with a single account. Adding it only once a second
// account appears would mean renaming the first account's entry at that point,
// and renaming a sync root re-registers it.
func autoSidebarLabel(acc account.Account, all []account.Account) string {
	if acc.LoginName == "" {
		return brand.Current.Name
	}
	for _, o := range all {
		if o.ID != acc.ID && strings.EqualFold(o.LoginName, acc.LoginName) {
			return brand.Current.Name + " - " + accountLabel(acc)
		}
	}
	return brand.Current.Name + " - " + acc.LoginName
}

// ownSidebarLabel is the label for our own entry (live mode), which points at
// the shown account's folder (GetBaseDir) and so is named after that account.
func (a *App) ownSidebarLabel() string {
	if a.eng == nil {
		return brand.Current.Name
	}
	return sidebarLabelFor(a.eng.Account)
}

// sidebarLabelFor is sidebarLabel against the configured accounts. The
// account is looked up again by ID because a running engine holds the record
// it started with, and the user can rename the entry since then.
func sidebarLabelFor(acc account.Account) string {
	_, st, _ := accountsAndDirs()
	if cur, ok := st.Find(acc.ID); ok {
		acc = cur
	}
	return sidebarLabel(acc, st.Accounts)
}

// maxSidebarName bounds a chosen name: Explorer shows a few dozen characters
// in the navigation pane, and anything longer is a mistake (a pasted path or
// paragraph), not a name.
const maxSidebarName = 80

// cleanSidebarName tidies a name typed in Settings: surrounding spaces go, and
// so do control characters (a pasted tab or newline would show as a box in
// Explorer). "" stays "", meaning the automatic name.
func cleanSidebarName(s string) (string, error) {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if utf8.RuneCountInString(s) > maxSidebarName {
		return "", errors.New("That name is too long, it can be 80 characters at most.")
	}
	return s, nil
}

// SetAccountSidebarName sets the name an account's Explorer sidebar entry is
// shown under ("" goes back to the automatic one) and renames the entry
// straight away. It returns "" or a message to show.
func (a *App) SetAccountSidebarName(id, name string) string {
	name, err := cleanSidebarName(name)
	if err != nil {
		return err.Error()
	}
	d, err := config.Resolve()
	if err != nil {
		return err.Error()
	}
	if err := account.Update(d.AccountsFile(), func(st *account.Store) error {
		acc, ok := st.Find(id)
		if !ok {
			return errors.New("That account is no longer set up on this PC.")
		}
		acc.Label = name
		st.Upsert(acc)
		return nil
	}); err != nil {
		return err.Error()
	}
	a.applySidebarLabel(id)
	a.emit("account")
	return ""
}

// applySidebarLabel renames the account's entries now rather than at the next
// start: each of its on-demand roots, and our own live-mode entry when it
// points at this account's folder (it follows the shown account).
func (a *App) applySidebarLabel(id string) {
	label := ""
	if _, st, ok := accountsAndDirs(); ok {
		if acc, found := st.Find(id); found {
			label = sidebarLabel(acc, st.Accounts)
		}
	}
	if label == "" {
		return
	}
	exe, _ := os.Executable()
	for dir, m := range a.mounts() {
		if m.accountID != id {
			continue
		}
		if err := cfapi.RenameSyncRoot(dir, label, exe); err != nil {
			slog.Warn("could not rename the folder's sidebar entry", "dir", dir, "err", err)
			continue
		}
		slog.Info("renamed the folder's sidebar entry", "dir", dir, "name", label)
	}
	if a.eng != nil && a.eng.Account.ID == id && a.GetSyncMode() != "ondemand" {
		a.syncSidebar() // rewrites our entry: its recorded name no longer matches
	}
}
