package config

import (
	"path/filepath"
	"testing"
)

// The detached-folder list records a share or mount that vanished from the
// account while its local copy was kept. It is what keeps that copy out of
// sync until the user decides what to do with it, so it must persist exactly.
func TestDetachedRoundTrip(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}.WithAccount("a")

	got, err := d.LoadDetached()
	if err != nil || len(got) != 0 {
		t.Fatalf("fresh: got %v, %v; want empty, nil", got, err)
	}

	team := Detached{LocalDir: filepath.FromSlash("/Users/x/Nextcloud"), RemoteRoot: "", Rel: "Team", AtUnix: 100, ParkedAt: filepath.FromSlash("/Users/x/Nextcloud - no longer shared/Team")}
	if err := d.AddDetached(team); err != nil {
		t.Fatal(err)
	}
	if err := d.AddDetached(Detached{LocalDir: filepath.FromSlash("/Users/x/Nextcloud"), Rel: "Projects/Group", AtUnix: 101}); err != nil {
		t.Fatal(err)
	}
	// The same copy again (however the paths are spelled) is a no-op.
	if err := d.AddDetached(Detached{LocalDir: filepath.FromSlash("/users/X/nextcloud/"), Rel: "Team", AtUnix: 999, ParkedAt: filepath.FromSlash("/users/X/nextcloud - NO LONGER SHARED/team")}); err != nil {
		t.Fatal(err)
	}

	got, err = d.LoadDetached()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != team || got[1].Rel != "Projects/Group" {
		t.Fatalf("round trip = %+v", got)
	}

	// Entries are addressed by where the copy IS: the parked path for a moved
	// copy, the sync-folder path for one kept in place.
	if err := d.RemoveDetached(filepath.FromSlash("/users/X/nextcloud - NO LONGER SHARED/team")); err != nil {
		t.Fatal(err)
	}
	got, _ = d.LoadDetached()
	if len(got) != 1 || got[0].Rel != "Projects/Group" {
		t.Fatalf("after remove = %+v", got)
	}
	if got[0].LocalPath() != filepath.FromSlash("/Users/x/Nextcloud/Projects/Group") {
		t.Errorf("in-place LocalPath = %q", got[0].LocalPath())
	}
	// Removing what is not there is not an error.
	if err := d.RemoveDetached(filepath.FromSlash("/elsewhere/Team")); err != nil {
		t.Fatal(err)
	}
	// The same folder unshared AGAIN lands at a new parked path: a second entry.
	again := Detached{LocalDir: filepath.FromSlash("/Users/x/Nextcloud"), Rel: "Team", ParkedAt: filepath.FromSlash("/Users/x/Nextcloud - no longer shared/Team (2)")}
	for i := 0; i < 2; i++ {
		if err := d.AddDetached(again); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ = d.LoadDetached(); len(got) != 2 {
		t.Fatalf("second parked copy: %+v", got)
	}
}

// The list is per account, like the pairs it refers to.
func TestDetachedIsPerAccount(t *testing.T) {
	base := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	a, b := base.WithAccount("a"), base.WithAccount("b")
	if err := a.AddDetached(Detached{LocalDir: `C:\A`, Rel: "Team"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.LoadDetached(); len(got) != 0 {
		t.Errorf("account b sees account a's entry: %+v", got)
	}
}
