package config

import (
	"os"
	"reflect"
	"testing"
)

// TestSettingsRoundTrip verifies settings persist and reload faithfully — the
// fields several features rely on (BaseDir, AllowedFilenames, SyncMode, …).
func TestSettingsRoundTrip(t *testing.T) {
	d := Dirs{Config: t.TempDir()}

	// A missing file loads as the zero value, not an error.
	got, err := d.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings (missing): %v", err)
	}
	if got.BaseDir != "" || len(got.AllowedFilenames) != 0 {
		t.Fatalf("missing settings should be zero-valued, got %+v", got)
	}

	want := Settings{
		BaseDir:          `E:\Nextcloud`,
		SyncMode:         "live",
		AllowedFilenames: []string{".htaccess", ".user.ini"},
		UploadKBps:       512,
		ConflictPolicy:   "keepboth",
		Theme:            "dark",
	}
	if err := d.SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err = d.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.BaseDir != want.BaseDir || got.SyncMode != want.SyncMode ||
		got.UploadKBps != want.UploadKBps || got.ConflictPolicy != want.ConflictPolicy ||
		got.Theme != want.Theme {
		t.Errorf("scalar fields didn't round-trip:\n got  %+v\n want %+v", got, want)
	}
	if !reflect.DeepEqual(got.AllowedFilenames, want.AllowedFilenames) {
		t.Errorf("AllowedFilenames = %v, want %v", got.AllowedFilenames, want.AllowedFilenames)
	}
}

// TestIgnoreRoundTrip verifies the global ignore list persists, and that
// comments and blank lines are dropped on load.
func TestIgnoreRoundTrip(t *testing.T) {
	d := Dirs{Config: t.TempDir()}

	if pats, err := d.LoadIgnore(); err != nil || pats != nil {
		t.Fatalf("missing ignore should be nil/no-error, got %v / %v", pats, err)
	}

	if err := d.SaveIgnore([]string{"node_modules", "*.log", "build/out"}); err != nil {
		t.Fatalf("SaveIgnore: %v", err)
	}
	pats, err := d.LoadIgnore()
	if err != nil {
		t.Fatalf("LoadIgnore: %v", err)
	}
	want := []string{"node_modules", "*.log", "build/out"}
	if !reflect.DeepEqual(pats, want) {
		t.Errorf("LoadIgnore = %v, want %v", pats, want)
	}

	// Manually write a file with comments + blanks; they must be filtered out.
	if err := os.WriteFile(d.IgnoreFile(), []byte("# a comment\n\nnode_modules\n  \n*.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pats, err = d.LoadIgnore()
	if err != nil {
		t.Fatalf("LoadIgnore (with comments): %v", err)
	}
	if !reflect.DeepEqual(pats, []string{"node_modules", "*.tmp"}) {
		t.Errorf("comments/blanks not filtered: got %v", pats)
	}
}
