package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// userDataDir returns the platform-appropriate root for variable application
// data (as opposed to configuration). The stdlib offers UserConfigDir and
// UserCacheDir but no data-dir equivalent, so we resolve it ourselves.
func userDataDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		// %LocalAppData% is the conventional home for per-machine app state.
		if v := os.Getenv("LocalAppData"); v != "" {
			return v, nil
		}
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support"), nil
	default: // linux, bsd, etc. — follow the XDG Base Directory spec.
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return v, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share"), nil
	}
	return os.UserConfigDir()
}
