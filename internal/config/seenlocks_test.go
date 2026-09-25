package config

import (
	"os"
	"testing"
	"time"
)

func TestSeenLocksRoundTrip(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}.WithAccount("acc1")
	if got := d.LoadSeenLocks(); len(got) != 0 {
		t.Fatalf("fresh install: got %v, want empty", got)
	}
	since := time.Unix(1790368000, 0)
	in := []SeenLock{
		{LocalDir: `C:\Nimbo`, Path: "Team/Budget.xlsx", Owner: "bob", OwnerDisplay: "Bob", OwnerType: 0, Since: since},
		{LocalDir: `C:\Nimbo`, Path: "notes.md", AppName: "Text", OwnerType: 1},
	}
	if err := d.SaveSeenLocks(in); err != nil {
		t.Fatal(err)
	}
	out := d.LoadSeenLocks()
	if len(out) != 2 || out[0].Path != "Team/Budget.xlsx" || out[0].OwnerDisplay != "Bob" ||
		!out[0].Since.Equal(since) || out[1].AppName != "Text" || out[1].OwnerType != 1 {
		t.Fatalf("round trip = %+v", out)
	}
	if err := d.SaveSeenLocks(nil); err != nil {
		t.Fatal(err)
	}
	if got := d.LoadSeenLocks(); len(got) != 0 {
		t.Errorf("after clearing: got %v, want empty", got)
	}
}

// Two accounts must never read each other's locks.
func TestSeenLocksAreScopedPerAccount(t *testing.T) {
	base := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	if err := base.WithAccount("a").SaveSeenLocks([]SeenLock{{LocalDir: "x", Path: "a.txt"}}); err != nil {
		t.Fatal(err)
	}
	if got := base.WithAccount("b").LoadSeenLocks(); len(got) != 0 {
		t.Errorf("account b read %v", got)
	}
}

func TestSeenLocksCorruptFileIsNotFatal(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}.WithAccount("acc1")
	if err := os.WriteFile(d.SeenLocksFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := d.LoadSeenLocks(); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}
