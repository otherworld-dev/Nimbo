package engine

import "testing"

func TestForbidden_Check(t *testing.T) {
	f := NewForbidden(
		[]string{".htaccess"},         // names (plus builtin)
		[]string{"con"},               // basenames
		[]string{"\\", ":"},           // chars
		[]string{"part", ".filepart"}, // exts (normalized to .part/.filepart)
		[]string{".user.ini"},         // allow: user permits this builtin-forbidden name
	)
	cases := []struct {
		name    string
		blocked bool
	}{
		{"notes.txt", false},
		{".htaccess", true},       // explicit name
		{".htpasswd", true},       // builtin fallback
		{".user.ini", false},      // builtin-forbidden but user-allowed
		{"CON.txt", true},         // forbidden basename, case-insensitive
		{"backup.part", true},     // forbidden extension
		{"data.filepart", true},   // forbidden extension with dot
		{"a:b.txt", true},         // forbidden character
		{"weird\\name", true},     // forbidden character
		{"normal.partial", false}, // .partial != .part
	}
	for _, c := range cases {
		_, got := f.Check(c.name)
		if got != c.blocked {
			t.Errorf("Check(%q) = %v, want %v", c.name, got, c.blocked)
		}
	}
}

func TestFilterBlocked(t *testing.T) {
	f := NewForbidden(nil, nil, nil, nil, nil) // builtin only (.htaccess etc.)
	actions := []Action{
		{Kind: ActUpload, Path: "notes.txt"},
		{Kind: ActUpload, Path: "dir/.htaccess"},
		{Kind: ActUpload, Path: "secret.txt"}, // will be blacklisted
		{Kind: ActDownload, Path: "remote.txt"},
	}
	blacklisted := func(rel string) bool { return rel == "secret.txt" }

	kept, blocked := FilterBlocked(actions, f, nil, blacklisted)

	if len(blocked) != 1 || blocked[0].Path != "dir/.htaccess" {
		t.Fatalf("blocked = %+v, want one .htaccess", blocked)
	}
	// kept should contain notes.txt and the download, but not .htaccess or the
	// blacklisted secret.txt.
	if len(kept) != 2 {
		t.Fatalf("kept = %+v, want 2", kept)
	}
	for _, a := range kept {
		if a.Path == "dir/.htaccess" || a.Path == "secret.txt" {
			t.Errorf("kept should not include %q", a.Path)
		}
	}
}

func TestFilterBlocked_Escaping(t *testing.T) {
	f := NewForbidden(nil, nil, nil, nil, nil)      // builtin: .htaccess/.htpasswd forbidden
	esc := NewEscaper(f, []string{".htaccess"}, "") // opt in .htaccess only

	// An opted-in forbidden name is kept (the executor escapes it); a forbidden
	// name that's NOT opted in is still blocked.
	kept, blocked := FilterBlocked([]Action{
		{Kind: ActUpload, Path: "dir/.htaccess"},
		{Kind: ActUpload, Path: "dir/.htpasswd"},
	}, f, esc, nil)
	if len(blocked) != 1 || blocked[0].Path != "dir/.htpasswd" {
		t.Fatalf("blocked = %+v, want only .htpasswd", blocked)
	}
	if len(kept) != 1 || kept[0].Path != "dir/.htaccess" {
		t.Fatalf("kept = %+v, want .htaccess (to be escaped)", kept)
	}

	// Collision: a real .htaccess.nimboesc already occupies the escaped name, so the
	// forbidden .htaccess is blocked rather than overwriting it; the real file syncs.
	kept, blocked = FilterBlocked([]Action{
		{Kind: ActUpload, Path: "dir/.htaccess"},
		{Kind: ActUpload, Path: "dir/.htaccess.nimboesc"},
	}, f, esc, nil)
	if len(blocked) != 1 || blocked[0].Path != "dir/.htaccess" {
		t.Fatalf("collision blocked = %+v, want .htaccess", blocked)
	}
	if len(kept) != 1 || kept[0].Path != "dir/.htaccess.nimboesc" {
		t.Fatalf("collision kept = %+v, want the real .nimboesc file", kept)
	}
}
