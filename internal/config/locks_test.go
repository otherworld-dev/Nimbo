package config

import (
	"os"
	"testing"
	"time"
)

func TestHeldLocksRoundTrip(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}

	if got, err := d.LoadHeldLocks(); err != nil || len(got) != 0 {
		t.Fatalf("fresh install: got %v, %v; want empty and no error", got, err)
	}

	in := []HeldLock{
		{Account: "adam", RemotePath: "Team/Budget.xlsx", Token: "files_lock/abc", Taken: time.Unix(1786235387, 0)},
		{Account: "adam", RemotePath: "notes.md", Token: "files_lock/def", Taken: time.Unix(1786235400, 0)},
	}
	if err := d.SaveHeldLocks(in); err != nil {
		t.Fatal(err)
	}
	out, err := d.LoadHeldLocks()
	if err != nil || len(out) != 2 {
		t.Fatalf("got %v, %v; want 2 entries", out, err)
	}
	if out[0].RemotePath != "Team/Budget.xlsx" || out[0].Token != "files_lock/abc" {
		t.Errorf("round-trip lost data: %+v", out[0])
	}
	if !out[0].Taken.Equal(in[0].Taken) {
		t.Errorf("Taken = %v, want %v", out[0].Taken, in[0].Taken)
	}

	// Saving an empty list clears the registry rather than leaving stale entries.
	if err := d.SaveHeldLocks(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.LoadHeldLocks(); len(got) != 0 {
		t.Errorf("after clearing: got %v, want empty", got)
	}
}

// A damaged registry must never stop the client starting. Losing track of a lock
// is bad; refusing to boot over a diagnostic side-file is worse.
func TestHeldLocksCorruptFileIsNotFatal(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	if err := os.WriteFile(d.LocksFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := d.LoadHeldLocks()
	if err != nil {
		t.Errorf("err = %v, want nil — a corrupt registry must be survivable", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
	// And it must be recoverable by writing over it.
	if err := d.SaveHeldLocks([]HeldLock{{Account: "adam", RemotePath: "a.txt"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.LoadHeldLocks(); len(got) != 1 {
		t.Errorf("after rewrite: got %v, want 1", got)
	}
}

// The registry is shared across accounts, so each entry has to say whose it is —
// a sweep must be able to leave another account's locks alone.
func TestHeldLocksKeepAccount(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	if err := d.SaveHeldLocks([]HeldLock{
		{Account: "adam", RemotePath: "a.txt"},
		{Account: "other", RemotePath: "b.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	out, _ := d.LoadHeldLocks()
	seen := map[string]string{}
	for _, l := range out {
		seen[l.Account] = l.RemotePath
	}
	if seen["adam"] != "a.txt" || seen["other"] != "b.txt" {
		t.Errorf("accounts not preserved: %+v", out)
	}
}
