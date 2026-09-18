package engine

import "testing"

func TestIgnore_Match(t *testing.T) {
	ig := NewIgnore([]string{"*.log", "node_modules", "build/out", "secret/"})
	cases := []struct {
		path string
		want bool
	}{
		{"notes.txt", false},
		{"app.log", true},                // *.log basename
		{"sub/dir/app.log", true},        // *.log at depth
		{"node_modules", true},           // exact segment
		{"node_modules/lib/x.js", true},  // under an ignored dir (ancestor match)
		{"src/node_modules/y.js", true},  // ignored segment at depth
		{"build/out", true},              // path pattern
		{"build/out/file.bin", true},     // under path pattern
		{"build/other.txt", false},       // build/out doesn't match build/other
		{"secret/passwords", true},       // trailing-slash dir pattern + ancestor
		{"a.tmp", true},                  // default pattern
		{"~$doc.docx", true},             // default pattern
		{".sync_abc123.db", true},        // official client journal (default)
		{".sync_abc123.db-wal", true},    // its WAL
		{"Docs/.owncloudsync.log", true}, // official client log at depth
		{"real-database.db", false},      // a genuine user .db is NOT ignored
	}
	for _, c := range cases {
		if got := ig.Match(c.path); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIgnore_Defaults(t *testing.T) {
	ig := NewIgnore(nil) // built-in defaults only
	shouldMatch := []string{
		"a.tmp", "doc~", "~$sheet.xlsx", ".~lock.file", "x.part",
		".DS_Store", "Thumbs.db", "sub/desktop.ini",
		".sync_abc.db", ".sync_abc.db-wal", "Docs/.nextcloudsync.log",
	}
	for _, p := range shouldMatch {
		if !ig.Match(p) {
			t.Errorf("default ignore should match %q", p)
		}
	}
	// Dependency/VCS dirs now sync by default (users exclude them via Exclusions if
	// they want); ordinary files must never match.
	shouldNotMatch := []string{
		"node_modules", "app/node_modules/lib/x.js", ".git", "repo/.git/config",
		"x/.svn/entries", "y/.hg/store",
		"src/index.js", "notes.txt", "gitfile.txt", "node_modules.txt",
	}
	for _, p := range shouldNotMatch {
		if ig.Match(p) {
			t.Errorf("default ignore should NOT match %q", p)
		}
	}
}

// An exact exclusion names ONE pair-relative path (and everything under it),
// unlike a pattern without "/" which matches that name at any depth. It is what
// keeps a detached share's kept copy out of sync without also hiding some
// other folder that happens to share its name.
func TestIgnore_Exact(t *testing.T) {
	ig := NewIgnore(nil)
	ig.AddExact("Team", "Projects/Group")
	for _, p := range []string{"Team", "Team/Budget.xlsx", "Team/sub/x", "Projects/Group", "Projects/Group/a"} {
		if !ig.Match(p) {
			t.Errorf("exact exclusion should match %q", p)
		}
	}
	for _, p := range []string{"Teams", "Projects/Team", "Group", "Projects/Groups/a", "other/Team/x"} {
		if ig.Match(p) {
			t.Errorf("exact exclusion should NOT match %q", p)
		}
	}
	var nilIg *Ignore
	nilIg.AddExact("Team") // a nil matcher stays a no-op
	if nilIg.Match("Team") {
		t.Error("nil matcher matched")
	}
}

func TestIgnore_FilterMaps(t *testing.T) {
	ig := NewIgnore([]string{"*.log"})
	local := map[string]LocalState{"a.txt": {Path: "a.txt"}, "b.log": {Path: "b.log"}}
	remote := map[string]RemoteState{"a.txt": {Path: "a.txt"}, "c.log": {Path: "c.log"}}
	ig.FilterLocal(local)
	ig.FilterRemote(remote)
	if _, ok := local["b.log"]; ok {
		t.Error("local b.log should be filtered")
	}
	if _, ok := remote["c.log"]; ok {
		t.Error("remote c.log should be filtered")
	}
	if len(local) != 1 || len(remote) != 1 {
		t.Errorf("expected 1 each, got local=%d remote=%d", len(local), len(remote))
	}
}
