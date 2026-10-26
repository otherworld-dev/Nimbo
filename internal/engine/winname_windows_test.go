//go:build windows

package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/transport"
)

// Deck #984, live mode: a server name ending in a space or a dot cannot exist
// on Windows; writing "Pilot Validation /doc.pdf" lands in "Pilot Validation",
// over the real folder's file of that name. The scan must not report such
// items, nor anything beneath them.
func TestRemoteScanSkipsNamesWindowsCannotHold(t *testing.T) {
	srv := &fakeServer{dirs: map[string][]transport.Entry{
		"": {
			d("", "eroot"),
			d("Pilot Validation", "e1"),
			d("Pilot Validation ", "e2"),
			fi("report.", "e3", 5),
		},
		"Pilot Validation":  {d("Pilot Validation", "e1"), fi("Pilot Validation/doc.pdf", "e4", 1)},
		"Pilot Validation ": {d("Pilot Validation ", "e2"), fi("Pilot Validation /doc.pdf", "e5", 2)},
	}}
	var mu sync.Mutex
	reported := map[string][]Unholdable{}
	got, err := RemoteScan(context.Background(), srv, "", ScanOpts{OnUnholdable: func(dir string, items []Unholdable) {
		mu.Lock()
		reported[dir] = items
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Skipped, but reported, so the user is told instead of the items going
	// quietly missing; every listed folder reports, an empty list included.
	if r := reported[""]; len(r) != 2 || r[0] != (Unholdable{"Pilot Validation ", true}) || r[1] != (Unholdable{"report.", false}) {
		t.Errorf("root reported %v, want the space folder and report.", r)
	}
	if r, ok := reported["Pilot Validation"]; !ok || len(r) != 0 {
		t.Errorf("listed folder reported %v (present=%v), want an empty report", r, ok)
	}
	for _, bad := range []string{"Pilot Validation ", "Pilot Validation /doc.pdf", "report."} {
		if _, ok := got[bad]; ok {
			t.Errorf("scan reported %q, a name Windows cannot hold", bad)
		}
	}
	if _, ok := got["Pilot Validation/doc.pdf"]; !ok {
		t.Error("the real folder's file is missing from the scan")
	}
	if n := srv.callsFor("Pilot Validation "); n != 0 {
		t.Errorf("listed the unholdable folder %d time(s); it must not be descended into", n)
	}
}

// An unchanged subtree is rebuilt from the baseline, which an older version
// may have filled with such a name. It must not come back that way either.
func TestRemoteScanDropsUnholdableNamesFromTheBaselineSubtree(t *testing.T) {
	srv := &fakeServer{dirs: map[string][]transport.Entry{
		"": {d("", "eroot"), d("a", "ea")},
	}}
	base := map[string]BaselineState{
		"a":               {Path: "a", IsDir: true, RemoteETag: "ea"},
		"a/ok.txt":        {Path: "a/ok.txt", RemoteETag: "e1"},
		"a/old /x.txt":    {Path: "a/old /x.txt", RemoteETag: "e2"},
		"a/old ":          {Path: "a/old ", IsDir: true, RemoteETag: "e3"},
		"a/trailing.dot.": {Path: "a/trailing.dot.", RemoteETag: "e4"},
	}
	got, err := RemoteScan(context.Background(), srv, "", ScanOpts{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"a/old ", "a/old /x.txt", "a/trailing.dot."} {
		if _, ok := got[bad]; ok {
			t.Errorf("baseline subtree brought back %q", bad)
		}
	}
	if _, ok := got["a/ok.txt"]; !ok {
		t.Error("a/ok.txt missing from the reused subtree")
	}
}

// The local walk skips the same names, so the two sides agree: a folder an
// older version wrote by such a name (through \\?\) is neither uploaded nor
// read as a local deletion.
func TestLocalScanSkipsNamesWindowsCannotHold(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Pilot Validation"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(`\\?\`+filepath.Join(root, "Pilot Validation "), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := LocalScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["Pilot Validation "]; ok {
		t.Error("local scan reported a name Windows cannot hold")
	}
	if _, ok := got["Pilot Validation"]; !ok {
		t.Error("the real folder is missing from the local scan")
	}
}
