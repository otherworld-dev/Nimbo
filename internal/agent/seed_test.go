package agent

import (
	"slices"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
)

// The dev-ignore migration must add the patterns once, and never re-impose one
// the user has since deleted.
func TestSeedDevIgnores(t *testing.T) {
	d := config.Dirs{Config: t.TempDir()}
	e := &Engine{dirs: d}

	e.seedDevIgnores()
	pats, err := d.LoadIgnore()
	if err != nil {
		t.Fatalf("LoadIgnore: %v", err)
	}
	for _, want := range devIgnores {
		if !slices.Contains(pats, want) {
			t.Errorf("after seeding, ignore list missing %q (got %v)", want, pats)
		}
	}
	s, _ := d.LoadSettings()
	if !s.DevIgnoresSeeded {
		t.Fatal("DevIgnoresSeeded flag not set")
	}

	// User opts back in by deleting a pattern — the migration must not return it.
	pats = slices.DeleteFunc(pats, func(p string) bool { return p == "node_modules" })
	if err := d.SaveIgnore(pats); err != nil {
		t.Fatalf("SaveIgnore: %v", err)
	}
	e.seedDevIgnores()
	pats, _ = d.LoadIgnore()
	if slices.Contains(pats, "node_modules") {
		t.Error("seeding re-added a pattern the user deleted")
	}
}

// Seeding must not duplicate patterns the user already has.
func TestSeedDevIgnoresNoDuplicates(t *testing.T) {
	d := config.Dirs{Config: t.TempDir()}
	if err := d.SaveIgnore([]string{"node_modules", "*.log"}); err != nil {
		t.Fatalf("SaveIgnore: %v", err)
	}
	e := &Engine{dirs: d}
	e.seedDevIgnores()
	pats, _ := d.LoadIgnore()
	n := 0
	for _, p := range pats {
		if p == "node_modules" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("node_modules appears %d times, want 1 (%v)", n, pats)
	}
	if !slices.Contains(pats, "*.log") {
		t.Error("user pattern *.log lost in seeding")
	}
}
