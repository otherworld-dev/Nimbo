package engine

import (
	"context"
	"testing"

	"github.com/otherworld/nimbo/internal/transport"
)

// sd/sf build entries inside a received share: every node of the share carries
// S, root and descendants alike (sampled from a live Nextcloud).
func sd(path, etag string) transport.Entry {
	return transport.Entry{Path: path, IsDir: true, ETag: etag, Permissions: "SRGDNVCK"}
}
func sf(path, etag string, size int64) transport.Entry {
	return transport.Entry{Path: path, IsDir: false, ETag: etag, Size: size, Permissions: "SRGDNVW"}
}

// The scan marks the TOP of a share or mount — the node on a mount whose parent
// is not — and nothing beneath it. That flag is what lets a later pass tell an
// unshare from a deletion once the share has vanished (KeepDetached).
func TestRemoteScanMarksMountRoots(t *testing.T) {
	f := &fakeServer{
		dirs: map[string][]transport.Entry{
			"":         {d("", "eroot"), sd("Team", "et"), d("own", "eo"), sf("Budget.xlsx", "eb", 9)},
			"Team":     {sd("Team", "et"), sd("Team/sub", "es"), sf("Team/f", "ef", 1)},
			"Team/sub": {sd("Team/sub", "es")},
			"own":      {d("own", "eo"), fi("own/f", "e1", 1)},
		},
		fail: map[string]error{},
	}
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"Team":        true,  // a folder shared with me
		"Budget.xlsx": true,  // a single file shared with me
		"own":         false, // my own folder
		"own/f":       false,
		"Team/sub":    false, // inside the share: its parent is on the same mount
		"Team/f":      false,
	}
	for p, w := range want {
		r, ok := out[p]
		if !ok {
			t.Fatalf("%q missing from scan", p)
		}
		if r.MountRoot != w {
			t.Errorf("MountRoot(%q) = %v, want %v", p, r.MountRoot, w)
		}
	}
}

// A pair rooted INSIDE a share (the root's own row carries S) has no mount
// roots of its own: every child is on the same mount as the root.
func TestRemoteScanNoMountRootsWhenTheRootIsOnAMount(t *testing.T) {
	f := &fakeServer{
		dirs: map[string][]transport.Entry{
			"Team":     {sd("Team", "et"), sd("Team/sub", "es"), sf("Team/f", "ef", 1)},
			"Team/sub": {sd("Team/sub", "es")},
		},
		fail: map[string]error{},
	}
	out, err := RemoteScan(context.Background(), f, "Team", ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for p, r := range out {
		if r.MountRoot {
			t.Errorf("%q marked as a mount root inside a share", p)
		}
	}
}

// When the root listing carries no row for the root itself, the parent's
// status is unknown and nothing is marked: never guess that a folder is
// detachable.
func TestRemoteScanUnknownRootMarksNothing(t *testing.T) {
	f := &fakeServer{
		dirs: map[string][]transport.Entry{
			"":     {sd("Team", "et")}, // no own row
			"Team": {sd("Team", "et"), sf("Team/f", "ef", 1)},
		},
		fail: map[string]error{},
	}
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if out["Team"].MountRoot {
		t.Error("Team marked with the root's own status unknown")
	}
}

// A cached listing holds children only (childrenOnly strips the directory's
// own row), so the parent's status must travel with the queued directory, not
// be re-read from the listing — or a warm scan would silently stop marking.
func TestRemoteScanMarksMountRootsFromACachedListing(t *testing.T) {
	f := &fakeServer{
		dirs: map[string][]transport.Entry{
			"":          {d("", "eroot"), d("own", "eo")},
			"own":       {d("own", "eo"), sd("own/Share", "es")},
			"own/Share": {sd("own/Share", "es"), sf("own/Share/f", "ef", 1)},
		},
		fail: map[string]error{},
	}
	cp := newFakeCheckpoint()
	scan(t, f, cp) // warms the cache for "own" (a plain dir; the share itself is never cached)
	if !cp.touched(cp.saves, "own") {
		t.Fatalf("own was not cached: %v", cp.saves)
	}
	out := scan(t, f, cp)
	if f.callsFor("own") != 1 {
		t.Fatalf("own fetched %d times, want 1 (second scan must come from the cache)", f.callsFor("own"))
	}
	if !out["own/Share"].MountRoot {
		t.Error("share under a cached parent not marked")
	}
	if out["own/Share/f"].MountRoot {
		t.Error("file inside the share marked")
	}
}

// A subtree the ETag prune reconstructs from the baseline keeps the flag the
// baseline recorded, so a pruned pass cannot erase it.
func TestRemoteScanReplayKeepsMountRoot(t *testing.T) {
	f := &fakeServer{
		dirs: map[string][]transport.Entry{
			"":    {d("", "eroot"), d("own", "eo")},
			"own": {d("own", "eo"), sd("own/Share", "es")},
			// never listed: pruned by the matching baseline etag
		},
		fail: map[string]error{},
	}
	base := map[string]BaselineState{
		"own/Share":        {Path: "own/Share", IsDir: true, RemoteETag: "es", MountRoot: true},
		"own/Share/f":      {Path: "own/Share/f", RemoteETag: "ef"},
		"own/Share/nested": {Path: "own/Share/nested", IsDir: true, RemoteETag: "en", MountRoot: true},
	}
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if f.callsFor("own/Share") != 0 {
		t.Fatal("own/Share was listed despite a matching baseline etag")
	}
	if !out["own/Share"].MountRoot {
		t.Error("freshly listed share root not marked")
	}
	if !out["own/Share/nested"].MountRoot {
		t.Error("replayed row lost its MountRoot flag")
	}
	if out["own/Share/f"].MountRoot {
		t.Error("replayed plain row gained a flag")
	}
}
