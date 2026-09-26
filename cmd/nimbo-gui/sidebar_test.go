package main

import "testing"

// What the "Show Nimbo in the Explorer sidebar" checkbox says when the user
// has never chosen. A cloud sync root's node is put in the navigation pane by
// Windows itself, so "on" is the truthful default there (issue #7: the box
// read "off" while the entry was plainly visible). Without a sync root only an
// entry an older unpackaged build left behind counts.
func TestSidebarWantedFrom(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name        string
		recorded    *bool
		cloudRoot   bool
		legacyEntry bool
		want        bool
	}{
		{"recorded on wins", &on, false, false, true},
		{"recorded off wins over a cloud root", &off, true, true, false},
		{"cloud root, nothing recorded", nil, true, false, true},
		{"live, nothing recorded, no entry", nil, false, false, false},
		{"live, nothing recorded, old entry present", nil, false, true, true},
	}
	for _, c := range cases {
		if got := sidebarWantedFrom(c.recorded, c.cloudRoot, c.legacyEntry); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
