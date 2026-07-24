//go:build !windows

// Package policy is Windows-only (registry-backed admin policy). Elsewhere it
// reports an unmanaged install with sign-out allowed.
package policy

// Policy mirrors the Windows type so cross-platform code compiles.
type Policy struct {
	Managed       bool
	ServerURL     string
	LockServer    bool
	AllowSignOut  bool
	LockBandwidth bool
	UploadKBps    int
	DownloadKBps  int
	SyncMode      string

	DisableNameEscaping bool
}

// Load reports an unmanaged policy off Windows.
func Load() Policy { return Policy{AllowSignOut: true} }
