package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/vfs"
)

// A long conversion leaves a breadcrumb every N entries and one at the end,
// with the rate so far, so slow never reads as hung in the log (Deck #510).
func TestAdoptBreadcrumb(t *testing.T) {
	clock := time.Unix(1000, 0)
	now := func() time.Time { return clock }
	var got []string
	cb := adoptBreadcrumb(10, now, func(done, total int, perSec float64) {
		got = append(got, fmt.Sprintf("%d/%d@%.0f", done, total, perSec))
	})
	for done := 1; done <= 25; done++ {
		clock = clock.Add(100 * time.Millisecond) // 10 entries a second
		cb(done, 25)
	}
	want := "10/25@10 20/25@10 25/25@10"
	if strings.Join(got, " ") != want {
		t.Errorf("breadcrumbs %v, want %s", got, want)
	}
}

// The resume record (Deck #500) round-trips per account and folder, and a
// record for another folder is dropped rather than replayed: a plan must never
// be applied to a folder it did not describe.
func TestAdoptResumeRecord(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	plan := vfs.ResumePlan([]vfs.Entry{
		{Rel: "c.txt", Action: vfs.ActionConflict, Size: 9, MTimeNanos: 1, RemoteRel: "c.txt"},
		{Rel: "k.txt", Action: vfs.ActionKeep, Size: 1, MTimeNanos: 1, RemoteRel: "k.txt"},
	}, nil)

	saveAdoptResume("acct1", `C:\Users\x\Nimbo`, "", plan)
	if got := loadAdoptResume("acct2", `C:\Users\x\Nimbo`, ""); got != nil {
		t.Fatalf("another account's record leaked: %v", got)
	}
	got := loadAdoptResume("acct1", `c:\users\x\Nimbo`, "")
	if len(got) != 1 || got[0].Rel != "c.txt" || got[0].Action != vfs.ActionConflict {
		t.Fatalf("loaded %v, want only the conflict (Keep is left to reconcile)", got)
	}

	if got := loadAdoptResume("acct1", `D:\Elsewhere`, ""); got != nil {
		t.Fatalf("record replayed into a different folder: %v", got)
	}
	if _, err := os.Stat(adoptResumeFile("acct1")); !os.IsNotExist(err) {
		t.Errorf("stale record for another folder not dropped (stat err = %v)", err)
	}

	saveAdoptResume("acct1", `C:\Users\x\Nimbo`, "", plan)
	clearAdoptResume("acct1")
	if got := loadAdoptResume("acct1", `C:\Users\x\Nimbo`, ""); got != nil {
		t.Fatalf("cleared record still loads: %v", got)
	}

	// A plan with nothing unfinished writes no record at all.
	saveAdoptResume("acct1", `C:\Users\x\Nimbo`, "", vfs.ResumePlan([]vfs.Entry{
		{Rel: "k.txt", Action: vfs.ActionKeep, RemoteRel: "k.txt"},
	}, nil))
	if _, err := os.Stat(adoptResumeFile("acct1")); !os.IsNotExist(err) {
		t.Errorf("record written for a plan with nothing owed (stat err = %v)", err)
	}
	if !strings.Contains(adoptResumeFile("acct1"), "vfs-adopt-resume-acct1") {
		t.Errorf("record not scoped per account: %s", adoptResumeFile("acct1"))
	}
}
