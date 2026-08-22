package main

import (
	"path/filepath"
	"testing"
)

// knownDir is the safety gate for the vfs state heal: a plain directory is only
// converted back into a cloud placeholder when the local baselines PROVE the
// server has it. A false positive here stamps "synced" onto something the
// server may not have, so the negative cases matter more than the positives.
func TestKnownDir(t *testing.T) {
	s := newEtagStore(filepath.Join(t.TempDir(), "etags.json"))
	s.setMany(map[string]string{
		"Documents":            "e1",
		"Documents/Report.doc": "e2",
		"Photos/2026/cat.jpg":  "e3",
	})

	for _, tc := range []struct {
		remote string
		want   bool
		why    string
	}{
		{"Documents", true, "has its own baseline"},
		{"/Documents/", true, "slashes are trimmed like every other accessor"},
		{"Photos", true, "no own baseline, but a child baseline proves it exists"},
		{"Photos/2026", true, "intermediate ancestor of a recorded file"},
		{"Photo", false, "prefix of a sibling name must NOT match (Photos)"},
		{"Documents/Report.doc", true, "a recorded path counts even if it is a file"},
		{"Backups", false, "nothing recorded beneath it"},
		{"", false, "the root is never the heal's business"},
	} {
		if got := s.knownDir(tc.remote); got != tc.want {
			t.Errorf("knownDir(%q) = %v, want %v (%s)", tc.remote, got, tc.want, tc.why)
		}
	}
}
