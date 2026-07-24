//go:build windows

// Package policy reads machine-wide administrative policy from the registry so
// an IT admin can lock down a managed deployment (Group Policy / Intune write
// the same keys via the bundled ADMX). Policies live under
// HKLM\SOFTWARE\Policies\Nimbo and OVERRIDE the user's own settings. They are a
// BUSINESS-tier capability — the app only applies them when a valid business
// licence is installed (see the caller's gate).
package policy

import "golang.org/x/sys/windows/registry"

const policyKey = `SOFTWARE\Policies\Nimbo`

// Policy is the set of admin-enforced settings. Zero value = nothing managed
// (and sign-out allowed), which is the correct default for unmanaged installs.
type Policy struct {
	Managed bool // any policy value is present

	ServerURL  string // enforced/preset Nextcloud server
	LockServer bool   // user cannot change the server (sign-in forced to ServerURL)

	AllowSignOut bool // false = sign-out hidden/blocked (kiosk-style)

	LockBandwidth bool // bandwidth limits are enforced and not user-editable
	UploadKBps    int
	DownloadKBps  int

	SyncMode string // "" = user's choice; "live"/"ondemand" forces the mode

	DisableNameEscaping bool // true = force-off the server-forbidden-name escaping opt-in
}

// Load reads the current machine policy. A missing key yields an unmanaged
// policy with sign-out allowed.
func Load() Policy { return loadFrom(registry.LOCAL_MACHINE, policyKey) }

// loadFrom reads policy from an explicit hive+path (Load uses HKLM; tests use a
// writable HKCU key).
func loadFrom(root registry.Key, path string) Policy {
	p := Policy{AllowSignOut: true}
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return p
	}
	defer k.Close()
	p.Managed = true

	if v, _, err := k.GetStringValue("ServerURL"); err == nil {
		p.ServerURL = v
	}
	p.LockServer = dword(k, "LockServer") == 1
	// AllowSignOut defaults to true; only an explicit 0 disables it.
	if v, _, err := k.GetIntegerValue("AllowSignOut"); err == nil {
		p.AllowSignOut = v != 0
	}
	if dword(k, "LockBandwidth") == 1 {
		p.LockBandwidth = true
		p.UploadKBps = dword(k, "UploadKBps")
		p.DownloadKBps = dword(k, "DownloadKBps")
	}
	if v, _, err := k.GetStringValue("SyncMode"); err == nil && (v == "live" || v == "ondemand") {
		p.SyncMode = v
	}
	p.DisableNameEscaping = dword(k, "DisableNameEscaping") == 1
	return p
}

func dword(k registry.Key, name string) int {
	v, _, err := k.GetIntegerValue(name)
	if err != nil {
		return 0
	}
	return int(v)
}
