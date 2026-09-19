package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/otherworld/nimbo/internal/transport"
)

// A failure is classified by what it is, never by words in its message: the
// message carries file paths and URLs, and a path with "401" in it (a photo
// IMG_4012.jpg, a scan 20240115.pdf) timing out read as a rejected password,
// which signs the user out and stops syncing (Deck #691).
func TestSyncErrKindGoesByTypeNotByWords(t *testing.T) {
	timeout := &url.Error{Op: "Propfind", URL: "https://example.org/remote.php/dav/files/u/Photos/IMG_4012.jpg",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a timeout on a path with 401 in it", fmt.Errorf("stat %q: %w", "Photos/IMG_4012.jpg", timeout), "offline"},
		{"a scan named after a date", fmt.Errorf("stat %q: %w", "Scans/20240115.pdf", transport.RetriesExhausted(4, timeout)), "offline"},
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

// "Offline" is for the network, not for every error without an HTTP status:
// a subfolder Windows won't let us read, or a database error, said "Offline".
func TestSyncErrKindKeepsOfflineForTheNetwork(t *testing.T) {
	denied := &os.PathError{Op: "open", Path: `E:\Nextcloud\Private`, Err: syscall.Errno(5)} // ERROR_ACCESS_DENIED
	exhausted := fmt.Errorf("remote scan: %w", transport.RetriesExhausted(4, errors.New("server returned 502 Bad Gateway")))
	dial := &url.Error{Op: "Propfind", URL: "https://example.org/remote.php/dav", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a local folder we may not read", fmt.Errorf("local scan: walk local root: %w", denied), "error"},
		{"a database error", errors.New("load baseline: database disk image is malformed"), "error"},
		{"retries that ran out", exhausted, "offline"},
		{"a refused connection", fmt.Errorf("stat %q: %w", "a.txt", dial), "offline"},
		{"a dropped connection", fmt.Errorf("download: %w", io.ErrUnexpectedEOF), "offline"},
	} {
		if got := syncErrKind(tc.err); got != tc.want {
			t.Errorf("%s: syncErrKind = %q, want %q", tc.name, got, tc.want)
		}
	}
}
