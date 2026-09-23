package main

import (
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/transport"
)

// GitHub #7: a colleague locked a document, the user edited it meanwhile, and
// once the colleague unlocked, the held upload parked the server's UNCHANGED
// copy as a conflicted copy — the lock and unlock had moved the ETag. With the
// content key recorded for the baseline, only a real edit counts.
func TestServerEditedSince(t *testing.T) {
	mt := time.Unix(1790160000, 0)
	synced := transport.Entry{Path: "d.txt", ETag: "e1", Size: 41, LastModified: mt, UploadTime: 1790160705}
	key := synced.ContentKey()

	unlocked := synced
	unlocked.ETag = "e3" // locked and unlocked since: new ETag, same version
	edited := synced
	edited.ETag, edited.UploadTime = "e4", 1790160846 // a colleague saved over it, same size and mtime
	noUploadTime := unlocked
	noUploadTime.UploadTime = 0

	for _, tc := range []struct {
		name    string
		base    string
		baseKey string
		cur     transport.Entry
		want    bool
	}{
		{"same ETag", "e1", key, synced, false},
		{"lock/unlock bump, same version", "e1", key, unlocked, false},
		{"real edit, same size and mtime", "e1", key, edited, true},
		{"no recorded key: the ETag decides", "e1", "", unlocked, true},
		{"server reports no upload time: the ETag decides", "e1", key, noUploadTime, true},
	} {
		if got := serverEditedSince(tc.base, tc.baseKey, tc.cur); got != tc.want {
			t.Errorf("%s: serverEditedSince = %v, want %v", tc.name, got, tc.want)
		}
	}
}
