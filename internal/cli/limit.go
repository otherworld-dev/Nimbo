package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/otherworld/nimbo/internal/config"
)

// cmdLimit views or sets bandwidth limits (KiB/s; 0 = unlimited).
//
//	nimbo limit                 show current limits
//	nimbo limit up <kbps>       set upload limit
//	nimbo limit down <kbps>     set download limit
//	nimbo limit up <k> down <k> set both
//	nimbo limit none            remove all limits
func cmdLimit(_ context.Context, args []string) error {
	d, err := dirs()
	if err != nil {
		return err
	}
	s, err := d.LoadSettings()
	if err != nil {
		return err
	}

	if len(args) == 0 || args[0] == "show" {
		fmt.Printf("Upload limit:   %s\nDownload limit: %s\n", limitStr(s.UploadKBps), limitStr(s.DownloadKBps))
		return nil
	}
	if args[0] == "none" {
		s.UploadKBps, s.DownloadKBps = 0, 0
		return saveLimits(d, s)
	}

	// Parse "up <kbps>" / "down <kbps>" pairs.
	for i := 0; i+1 < len(args); i += 2 {
		v, err := strconv.Atoi(args[i+1])
		if err != nil || v < 0 {
			return fmt.Errorf("limit must be a non-negative number of KiB/s")
		}
		switch args[i] {
		case "up", "upload":
			s.UploadKBps = v
		case "down", "download":
			s.DownloadKBps = v
		default:
			return fmt.Errorf("unknown limit %q (use up|down|none)", args[i])
		}
	}
	return saveLimits(d, s)
}

func saveLimits(d config.Dirs, s config.Settings) error {
	if err := d.SaveSettings(s); err != nil {
		return err
	}
	fmt.Printf("Limits set — upload: %s, download: %s (applies to new syncs)\n",
		limitStr(s.UploadKBps), limitStr(s.DownloadKBps))
	return nil
}

func limitStr(kbps int) string {
	if kbps <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d KiB/s", kbps)
}
