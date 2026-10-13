package main

import (
	"strings"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/vfs"
)

// engineSaying is a stand-in for one account's Engine.SyncFirst: it answers
// with r and counts how often it was asked.
func engineSaying(r agent.SyncFirstResult, asked *int) func(string) agent.SyncFirstResult {
	return func(string) agent.SyncFirstResult { *asked++; return r }
}

func TestRouteSyncFirst(t *testing.T) {
	const path = `C:\Sync\D\report.docx`
	outside := agent.SyncFirstResult{Kind: agent.SyncFirstOutside}
	for _, c := range []struct {
		name    string
		paused  bool
		mounted bool
		mount   *vfs.SyncFirstResult // nil = its watcher is not running
		live    []agent.SyncFirstResult
		want    syncFirstOutcome
		why     string
		// how many of the live engines, and whether the mount, should be asked
		wantLiveAsked  int
		wantMountAsked bool
	}{
		{name: "a large file waiting in the lane",
			live: []agent.SyncFirstResult{{Kind: agent.SyncFirstAsked, Lane: 1}}, want: sfAsked, wantLiveAsked: 1},
		{name: "a live file handed to a quick pass",
			live: []agent.SyncFirstResult{{Kind: agent.SyncFirstAsked, Paths: 1}}, want: sfAsked, wantLiveAsked: 1},
		{name: "a live file that has gone since the menu opened",
			live: []agent.SyncFirstResult{{Kind: agent.SyncFirstGone}}, want: sfAsked, wantLiveAsked: 1},
		{name: "the second account's folder",
			live: []agent.SyncFirstResult{outside, {Kind: agent.SyncFirstAsked, Paths: 1}, outside}, want: sfAsked, wantLiveAsked: 2},
		{name: "outside every sync folder",
			live: []agent.SyncFirstResult{outside, outside}, want: sfOutside, wantLiveAsked: 2},
		{name: "outside, with no account running", want: sfOutside},
		{name: "live, paused",
			paused: true, live: []agent.SyncFirstResult{{Kind: agent.SyncFirstPaused}}, want: sfPaused, wantLiveAsked: 1},
		{name: "live, an ignored file",
			live: []agent.SyncFirstResult{{Kind: agent.SyncFirstNotSynced}}, want: sfNotSynced, wantLiveAsked: 1},
		{name: "live, a folder held back",
			live: []agent.SyncFirstResult{{Kind: agent.SyncFirstHeld, Why: "It overlaps another folder."}},
			want: sfHeld, why: "It overlaps another folder.", wantLiveAsked: 1},

		{name: "on-demand, an upload started",
			mounted: true, mount: &vfs.SyncFirstResult{Uploads: 1}, want: sfAsked, wantMountAsked: true},
		{name: "on-demand, a kept file's download moved up",
			mounted: true, mount: &vfs.SyncFirstResult{Downloads: 1}, want: sfAsked, wantMountAsked: true},
		{name: "on-demand, already in step",
			mounted: true, mount: &vfs.SyncFirstResult{}, want: sfAsked, wantMountAsked: true},
		{name: "on-demand, an online-only file",
			mounted: true, mount: &vfs.SyncFirstResult{OnlineOnly: true}, want: sfOnlineOnly, wantMountAsked: true},
		{name: "on-demand, a temporary file",
			mounted: true, mount: &vfs.SyncFirstResult{NotSynced: true}, want: sfNotSynced, wantMountAsked: true},
		{name: "on-demand, paused: the watcher is not even asked",
			paused: true, mounted: true, mount: &vfs.SyncFirstResult{Uploads: 1}, want: sfPaused},
		{name: "on-demand, the watcher itself says paused",
			mounted: true, mount: &vfs.SyncFirstResult{Paused: true}, want: sfPaused, wantMountAsked: true},
		{name: "on-demand, folder still being set up", mounted: true, want: sfNotReady},
		// An account's on-demand folder can also be on record as a sync
		// folder of its engine, which does not sync it in that mode.
		{name: "on-demand wins over an engine that lists the same folder",
			mounted: true, mount: &vfs.SyncFirstResult{Uploads: 1},
			live: []agent.SyncFirstResult{{Kind: agent.SyncFirstAsked, Paths: 1}}, want: sfAsked, wantMountAsked: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			liveAsked, mountAsked := 0, false
			var live []func(string) agent.SyncFirstResult
			for _, r := range c.live {
				live = append(live, engineSaying(r, &liveAsked))
			}
			var mount func(string) vfs.SyncFirstResult
			if c.mount != nil {
				mount = func(got string) vfs.SyncFirstResult {
					if got != path {
						t.Errorf("the watcher was asked for %q, want %q", got, path)
					}
					mountAsked = true
					return *c.mount
				}
			}
			got, why := routeSyncFirst(path, c.paused, c.mounted, mount, live)
			if got != c.want || why != c.why {
				t.Errorf("got outcome %d (%q), want %d (%q)", got, why, c.want, c.why)
			}
			if liveAsked != c.wantLiveAsked {
				t.Errorf("%d engine(s) were asked, want %d", liveAsked, c.wantLiveAsked)
			}
			if mountAsked != c.wantMountAsked {
				t.Errorf("watcher asked = %v, want %v", mountAsked, c.wantMountAsked)
			}
		})
	}
}

func TestSyncFirstMessage(t *testing.T) {
	// Nothing to say when the request was simply handed over, and the
	// "outside" case is a dialog, not a toast.
	for _, o := range []syncFirstOutcome{sfAsked, sfOutside} {
		if title, text := syncFirstMessage("a.txt", o, ""); title != "" || text != "" {
			t.Errorf("outcome %d: got %q / %q, want nothing", o, title, text)
		}
	}
	for o, want := range map[syncFirstOutcome]string{
		sfPaused:     "paused",
		sfNotSynced:  "left out of syncing",
		sfOnlineOnly: "Always keep on this device",
		sfNotReady:   "being set up",
	} {
		title, text := syncFirstMessage("a.txt", o, "")
		if title == "" || !strings.Contains(title+" "+text, want) || !strings.Contains(text, "a.txt") {
			t.Errorf("outcome %d: got %q / %q, want the file's name and %q", o, title, text, want)
		}
	}
	if _, text := syncFirstMessage("Photos", sfHeld, "It overlaps another folder."); !strings.HasSuffix(text, "It overlaps another folder.") {
		t.Errorf("held: the reason is missing from %q", text)
	}
}

// The menu runs once per selected item, so one selection must not raise the
// same toast for every file in it.
func TestSyncFirstToastsAreSpacedOut(t *testing.T) {
	a := &App{}
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	if !a.syncFirstMaySay("Syncing is paused", now) {
		t.Fatal("the first toast was held back")
	}
	if a.syncFirstMaySay("Syncing is paused", now.Add(time.Second)) {
		t.Fatal("the same toast was shown again a second later")
	}
	if !a.syncFirstMaySay("Not synced", now.Add(time.Second)) {
		t.Fatal("a different toast was held back")
	}
	if !a.syncFirstMaySay("Syncing is paused", now.Add(syncFirstToastEvery)) {
		t.Fatal("the toast was still held back after the wait")
	}
}

// Started by the menu, with no engine yet: every path asked for is kept, not
// just the last one, and handed back once.
func TestSyncFirstBeforeTheEngineIsReady(t *testing.T) {
	a := &App{}
	a.syncFirstLocalPath(`C:\Sync\a.txt`)
	a.syncFirstLocalPath(`C:\Sync\b.txt`)
	a.syncFirstLocalPath("") // an empty argument is nothing
	got := a.takePendingSyncFirst()
	if len(got) != 2 || got[0] != `C:\Sync\a.txt` || got[1] != `C:\Sync\b.txt` {
		t.Fatalf("pending = %v", got)
	}
	if again := a.takePendingSyncFirst(); len(again) != 0 {
		t.Fatalf("the pending paths were handed back twice: %v", again)
	}
	for i := 0; i < pendingSyncFirstMax+10; i++ {
		a.queueSyncFirst(`C:\Sync\many.txt`)
	}
	if n := len(a.takePendingSyncFirst()); n != pendingSyncFirstMax {
		t.Fatalf("kept %d pending paths, want at most %d", n, pendingSyncFirstMax)
	}
}

// The menu's argument reaches the handler from a second launch and from the
// launch that started the app.
func TestSyncFirstArgument(t *testing.T) {
	if got := argValue([]string{"nimbo-gui.exe", "--sync-first", `C:\Sync\a b.txt`}, "--sync-first"); got != `C:\Sync\a b.txt` {
		t.Fatalf("got %q", got)
	}
	// The other entries' arguments are not mistaken for it.
	for _, other := range []string{"--share", "--versions", "--keep", "--free"} {
		if got := argValue([]string{"nimbo-gui.exe", other, `C:\Sync\a.txt`}, "--sync-first"); got != "" {
			t.Fatalf("%s was read as --sync-first: %q", other, got)
		}
	}
}
