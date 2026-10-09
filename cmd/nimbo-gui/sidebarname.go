package main

import (
	"strings"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/brand"
)

// sidebarLabel is the name an account's Explorer sidebar entry is shown under:
// the product name and the login, "Nimbo - adam", so that with several
// accounts each entry says whose it is (GitHub #10, #19). Two accounts with the
// same login on different servers would still look alike, so then the server
// is added as well: "Nimbo - adam on cloud.example.com".
//
// The login is shown even with a single account. Adding it only once a second
// account appears would mean renaming the first account's entry at that point,
// and renaming a sync root re-registers it.
func sidebarLabel(acc account.Account, all []account.Account) string {
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

// sidebarLabelFor is sidebarLabel against the configured accounts.
func sidebarLabelFor(acc account.Account) string {
	_, st, _ := accountsAndDirs()
	return sidebarLabel(acc, st.Accounts)
}
