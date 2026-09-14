// Package account manages Nextcloud accounts: the browser-based Login Flow v2,
// secure storage of the resulting app password in the OS keychain, and a small
// JSON store of non-secret account metadata.
package account

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Account holds the non-secret metadata for a single Nextcloud login. The app
// password itself is never stored here — it lives in the OS keychain, keyed by
// the account ID (see keychain.go).
type Account struct {
	ID        string `json:"id"`        // stable identifier, derived from server + user
	ServerURL string `json:"serverURL"` // base URL, no trailing slash
	LoginName string `json:"loginName"` // the Nextcloud login/user name
	// Local is the optional local network route: an address Nimbo dials
	// instead of ServerURL's host while it is reachable and proves to be the
	// same server. nil = off. Only ever set on https:// accounts. Spec:
	// docs/specs/2026-09-13-local-network-route-design.md.
	Local *LocalRoute `json:"local,omitempty"`
}

// LocalRoute is the saved setup of an account's local network address.
type LocalRoute struct {
	Address string `json:"address"`       // host or host:port (IPv6 in brackets); no scheme, no path
	Pin     string `json:"pin,omitempty"` // lowercase hex SHA-256 of the leaf certificate; "" = verify via the OS store
	RootID  string `json:"rootID"`        // oc:id of the DAV root, captured over the public URL at setup
}

// newID derives a stable, filesystem- and keychain-safe identifier from the
// server URL and login name, so the same account always maps to the same
// keychain entry and state database.
func newID(serverURL, loginName string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(serverURL) + "|" + loginName))
	return hex.EncodeToString(sum[:8])
}

// DAVRoot returns the WebDAV files endpoint for this account.
func (a Account) DAVRoot() string {
	return a.ServerURL + "/remote.php/dav/files/" + a.LoginName
}
