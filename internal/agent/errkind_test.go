package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/otherworld/nimbo/internal/transport"
)

// A failure is classified by what it is, never by words in its message: the
// message carries file paths and URLs, and a path with "401" in it (a photo
// IMG_4012.jpg, a scan 20240115.pdf) timing out read as a rejected password,
// which signs the user out and stops syncing (Deck #691).
func TestSyncErrKindGoesByTypeNotByWords(t *testing.T) {
	timeout := errors.New("dial tcp 192.168.5.252:443: i/o timeout")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a timeout on a path with 401 in it", fmt.Errorf("stat %q: %w", "Photos/IMG_4012.jpg", timeout), "offline"},
		{"a scan named after a date", fmt.Errorf("stat %q: request failed after 4 attempts: %w", "Scans/20240115.pdf", timeout), "offline"},
		{"a real 401", fmt.Errorf("remote scan: %w", &transport.StatusError{Op: "PROPFIND", Path: "x", Code: 401, Status: "401 Unauthorized"}), "auth"},
		{"an OCS 401", fmt.Errorf("share list: %w", transport.ErrUnauthorized), "auth"},
		{"a refusal on a path with 401 in it", &transport.StatusError{Op: "PROPFIND", Path: "Docs/unauthorized-401.txt", Code: 403, Status: "403 Forbidden"}, "error"},
		{"a context deadline", fmt.Errorf("stat: %w", context.DeadlineExceeded), "offline"},
	} {
		if got := syncErrKind(tc.err); got != tc.want {
			t.Errorf("%s: syncErrKind = %q, want %q", tc.name, got, tc.want)
		}
	}
}
