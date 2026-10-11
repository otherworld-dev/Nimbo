package main

import (
	"reflect"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transport"
)

// The folder picker's rows: the listed folder itself dropped, ignored names
// never shown, folders before files, A-Z ignoring case, sizes carried through.
func TestBrowseEntriesFiltersSortsAndSizes(t *testing.T) {
	listing := []transport.Entry{
		{Path: "Work", IsDir: true, Size: 900},
		{Path: "Work/zeta", IsDir: true, Size: 10},
		{Path: "Work/.git", IsDir: true, Size: 5},
		{Path: "Work/Alpha", IsDir: true, Size: 20},
		{Path: "Work/beta", IsDir: true, Size: 30},
		{Path: "Work/desktop.ini", Size: 1},
		{Path: "Work/.nextcloudsync.log", Size: 2},
		{Path: "Work/notes.txt", Size: 3},
		{Path: "Work/build", IsDir: true, Size: 40},
	}
	ig := engine.NewIgnore([]string{".git", "build/out"})
	rel := pairRelFunc([]config.SyncPair{{LocalDir: `C:\Sync`, RemoteRoot: ""}})
	got := browseEntries("/Work/", listing, rel, ig.Match)
	want := []BrowseEntry{
		{Name: "Alpha", Path: "Work/Alpha", IsDir: true, Size: 20},
		{Name: "beta", Path: "Work/beta", IsDir: true, Size: 30},
		{Name: "build", Path: "Work/build", IsDir: true, Size: 40},
		{Name: "zeta", Path: "Work/zeta", IsDir: true, Size: 10},
		{Name: "notes.txt", Path: "Work/notes.txt", Size: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// A pattern with a "/" is matched against the PAIR-relative path, as the
// engine does: "build/out" hides <pair root>/build/out, not a "build/out"
// that merely sits somewhere else in the account.
func TestBrowseEntriesIgnoresPairRelative(t *testing.T) {
	ig := engine.NewIgnore([]string{"build/out"})
	rel := pairRelFunc([]config.SyncPair{
		{LocalDir: `C:\All`, RemoteRoot: ""},
		{LocalDir: `C:\Proj`, RemoteRoot: "/Code/Proj/"},
	})
	inPair := browseEntries("Code/Proj/build", []transport.Entry{
		{Path: "Code/Proj/build/out", IsDir: true},
		{Path: "Code/Proj/build/keep", IsDir: true},
	}, rel, ig.Match)
	if len(inPair) != 1 || inPair[0].Name != "keep" {
		t.Errorf("inside the Proj pair: got %+v, want only keep", inPair)
	}
	elsewhere := browseEntries("Other/build", []transport.Entry{
		{Path: "Other/build/out", IsDir: true},
	}, rel, ig.Match)
	if len(elsewhere) != 1 {
		t.Errorf("Other/build/out is pair-relative Other/build/out in the whole-account pair, not build/out: got %+v", elsewhere)
	}
	top := browseEntries("", []transport.Entry{{Path: "build", IsDir: true}}, rel, ig.Match)
	if len(top) != 1 {
		t.Errorf("a top-level build folder is not build/out: got %+v", top)
	}
}

func TestPairRelFunc(t *testing.T) {
	rel := pairRelFunc([]config.SyncPair{
		{RemoteRoot: "Docs"},
		{RemoteRoot: "Docs/Deep"},
	})
	cases := map[string]string{
		"Docs/a":        "a",
		"Docs/Deep/b":   "b",
		"Docs":          "",
		"DocsOther/c":   "DocsOther/c", // a name that only starts with a root is not inside it
		"Elsewhere/d/e": "Elsewhere/d/e",
	}
	for in, want := range cases {
		if got := rel(in); got != want {
			t.Errorf("rel(%q) = %q, want %q", in, got, want)
		}
	}
}
