package main

import (
	"io"
	"math"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const mb = int64(1) << 20

// The flyout's one status line, in priority order: needs you, paused,
// offline, busy, idle (Deck #988). Every case pins the state, both lines and
// the Status tab a click opens.
func TestComposeNow(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	up := func(path string, size, sent int64) NowJobDTO {
		return NowJobDTO{Kind: "upload", Path: path, Size: size, Sent: sent, State: "running"}
	}
	down := func(path string) NowJobDTO {
		return NowJobDTO{Kind: "download", Path: path, Size: 10 * mb, State: "running"}
	}
	for _, tc := range []struct {
		name      string
		in        nowInput
		state     string
		main      string
		sub       string // exact, unless subHas/subStart is set
		subHas    string
		subStart  string
		tab       string
		wantProg  float64
		checkProg bool
	}{
		{name: "idle",
			in:    nowInput{LastCheck: now.Add(-time.Minute)},
			state: "idle", main: "Up to date", sub: "Checked 1 min ago", tab: "activity"},
		{name: "idle, incomplete",
			in:    nowInput{LastCheck: now.Add(-time.Hour), CheckIncomplete: true},
			state: "idle", main: "Up to date", sub: "Some folders couldn't be checked", tab: "activity"},
		{name: "one upload",
			in:    nowInput{Jobs: []NowJobDTO{up("TerraVia/Survey-North.tif", 2150*mb, 340*mb)}, LastCheck: now.Add(-time.Minute)},
			state: "busy", main: "Uploading Survey-North.tif", subHas: "1 upload", tab: "activity",
			checkProg: true, wantProg: 0.16},
		{name: "two uploads and a download",
			in:    nowInput{Jobs: []NowJobDTO{up("a.txt", 10, 1), up("b.txt", 10, 1), down("c.txt")}},
			state: "busy", main: "Uploading a.txt", subStart: "2 uploads, 1 download", tab: "activity"},
		{name: "checking only",
			in:    nowInput{Checking: []NowCheckDTO{{Dirs: 1240}}, LastCheck: now.Add(-time.Minute)},
			state: "busy", main: "Checking folders… 1,240", tab: "activity", sub: ""},
		{name: "needs you while busy",
			in: nowInput{Attention: AttentionInfo{Blocked: 2, Conflicts: 1},
				Jobs: []NowJobDTO{up("a.txt", 10, 1)}},
			state: "attention", main: "3 items need you", sub: "2 can't sync, 1 conflict", tab: "conflicts"},
		{name: "needs you, blocked only",
			in:    nowInput{Attention: AttentionInfo{Blocked: 1}},
			state: "attention", main: "1 item needs you", sub: "1 can't sync", tab: "blocked"},
		{name: "needs you, detached only",
			in:    nowInput{Attention: AttentionInfo{Detached: 2}},
			state: "attention", main: "2 items need you", sub: "2 folders no longer shared", tab: "detached"},
		{name: "paused with conflicts",
			in:    nowInput{Paused: true, Attention: AttentionInfo{Conflicts: 1}},
			state: "attention", main: "1 item needs you", sub: "1 conflict", tab: "conflicts"},
		{name: "paused",
			in:    nowInput{Paused: true, Waiting: 4},
			state: "paused", main: "Paused", sub: "4 changes waiting", tab: "activity"},
		{name: "paused until",
			in:    nowInput{Paused: true, PauseUntil: "18:00"},
			state: "paused", main: "Paused", sub: "until 18:00", tab: "activity"},
		{name: "offline",
			in:    nowInput{Offline: true, Waiting: 3},
			state: "offline", main: "Can't reach the server", sub: "3 changes waiting to upload", tab: "activity"},
		{name: "first sync waiting for a choice",
			in:    nowInput{FirstSyncWaiting: true, Jobs: []NowJobDTO{down("big/map.qgz")}},
			state: "busy", main: "Downloading map.qgz", subHas: "Choose folders to download first", tab: "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Now = now
			got := composeNow(tc.in, "Nimbo")
			if got.State != tc.state || got.Main != tc.main || got.Tab != tc.tab {
				t.Errorf("got state=%q main=%q tab=%q, want %q %q %q", got.State, got.Main, got.Tab, tc.state, tc.main, tc.tab)
			}
			switch {
			case tc.subHas != "":
				if !strings.Contains(got.Sub, tc.subHas) {
					t.Errorf("sub = %q, want it to contain %q", got.Sub, tc.subHas)
				}
			case tc.subStart != "":
				if !strings.HasPrefix(got.Sub, tc.subStart) {
					t.Errorf("sub = %q, want it to start %q", got.Sub, tc.subStart)
				}
			default:
				if got.Sub != tc.sub {
					t.Errorf("sub = %q, want %q", got.Sub, tc.sub)
				}
			}
			if tc.checkProg && math.Abs(got.Progress-tc.wantProg) > 0.01 {
				t.Errorf("progress = %v, want ≈ %v", got.Progress, tc.wantProg)
			}
		})
	}
}

// A job of another account never drives the shown account's line; it says so
// on the second line instead.
func TestComposeNowMentionsOtherAccount(t *testing.T) {
	got := composeNow(nowInput{ShownAccount: "me", OtherBusy: "Work",
		Jobs: []NowJobDTO{{Account: "Work", Kind: "upload", Path: "x.txt", State: "running"}},
		Now:  time.Now()}, "Nimbo")
	if got.State != "idle" || got.Main != "Up to date" || got.Sub != "Work account syncing" {
		t.Errorf("got %q / %q / %q", got.State, got.Main, got.Sub)
	}
	if len(got.Jobs) != 1 {
		t.Errorf("jobs = %d, want the other account's job still listed", len(got.Jobs))
	}
}

func TestComposeNowAddsInUseAndLarge(t *testing.T) {
	got := composeNow(nowInput{InUse: 1, LargeRunning: 2, Now: time.Now()}, "Nimbo")
	for _, want := range []string{"1 file in use", "2 large files syncing"} {
		if !strings.Contains(got.Sub, want) {
			t.Errorf("sub = %q, want it to contain %q", got.Sub, want)
		}
	}
}

// A line never names internal kinds, and a check with no transfers has no
// progress bar.
func TestComposeNowNoProgressWithoutTotal(t *testing.T) {
	got := composeNow(nowInput{Checking: []NowCheckDTO{{Dirs: 3}}, Now: time.Now()}, "Nimbo")
	if got.Progress != -1 {
		t.Errorf("progress = %v, want -1", got.Progress)
	}
}

// Explorer reads one file in several ranges at once: one job, bytes summed.
func TestHydrateJobsMergeRanges(t *testing.T) {
	var h hydrateJobs
	a := h.begin("acct", "Docs/Map.qgz", 4*mb)
	b := h.begin("acct", "Docs/Map.qgz", 8*mb)
	a.add(mb)
	b.add(mb)
	snap := h.snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %d jobs, want 1: %+v", len(snap), snap)
	}
	if j := snap[0]; j.Sent != 2*mb || j.Size != 8*mb || j.Kind != "download" || j.Path != "Docs/Map.qgz" || j.Account != "acct" {
		t.Errorf("job = %+v", j)
	}
	a.end()
	if len(h.snapshot()) != 1 {
		t.Errorf("one range still open: the job should stay")
	}
	b.end()
	b.end() // twice is harmless
	if n := len(h.snapshot()); n != 0 {
		t.Errorf("snapshot after both ends = %d jobs, want 0", n)
	}
}

func TestNowEmitterThrottles(t *testing.T) {
	var emits atomic.Int32
	var lastEmit atomic.Int64
	e := newNowEmitter(50*time.Millisecond, func() {
		emits.Add(1)
		lastEmit.Store(time.Now().UnixNano())
	})
	start := time.Now()
	for i := 0; i < 100; i++ {
		e.nudge()
		time.Sleep(time.Millisecond)
	}
	lastNudge := time.Now()
	spent := lastNudge.Sub(start)
	time.Sleep(200 * time.Millisecond)
	// 100 ms of nudges at one emit per 50 ms: 2 to 4 emits. A loaded machine
	// can stretch the nudging, and the bound stretches with it.
	max := int32(4)
	if m := int32(spent/(50*time.Millisecond)) + 2; m > max {
		max = m
	}
	if n := emits.Load(); n < 2 || n > max {
		t.Errorf("emits = %d over %v of nudges, want 2..%d", n, spent, max)
	}
	if time.Unix(0, lastEmit.Load()).Before(lastNudge) {
		t.Errorf("no emit after the last nudge")
	}
}

// The engine's own status line still counts: a stopped sync is not "Up to
// date", work no job explains is busy, and uploads held for a program are
// not "Up to date" either.
func TestComposeNowReadsEngineStatus(t *testing.T) {
	for _, tc := range []struct {
		status      string
		state, main string
	}{
		{"Offline", "offline", "Can't reach the server"},
		{"Can't reach your server — retrying", "offline", "Can't reach the server"},
		{"Sync stopped — local folder missing", "offline", "Sync stopped — local folder missing"},
		{"Error", "offline", "Error"},
		{"Sign in again", "offline", "Sign in again"},
		{"Syncing…", "busy", "Syncing…"},
		{"Moving your folder…", "busy", "Moving your folder…"},
		{"Waiting — Report.docx is open in another program", "idle", "Waiting — Report.docx is open in another program"},
		{"Up to date", "idle", "Up to date"},
	} {
		in := nowInput{Now: time.Now()}
		in.Offline, in.Problem, in.Working, in.Holding = classifyStatus(tc.status)
		got := composeNow(in, "Nimbo")
		if got.State != tc.state || got.Main != tc.main {
			t.Errorf("status %q: got %q / %q, want %q / %q", tc.status, got.State, got.Main, tc.state, tc.main)
		}
	}
}

func TestMountPathRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Nimbo")
	mounts := map[string]*odMount{dir: {remoteRoot: ""}}
	abs := filepath.Join(dir, "TerraVia", "Survey-North.tif")
	if got := mountPath(mounts, abs); got != "TerraVia/Survey-North.tif" {
		t.Errorf("mountPath = %q", got)
	}
	if got := mountLocal(mounts, "TerraVia/Survey-North.tif"); pathKey(got) != pathKey(abs) {
		t.Errorf("mountLocal = %q, want %q", got, abs)
	}
	sub := map[string]*odMount{dir: {remoteRoot: "/Work/"}}
	if got := mountPath(sub, abs); got != "Work/TerraVia/Survey-North.tif" {
		t.Errorf("mountPath under a remote root = %q", got)
	}
	if got := mountLocal(sub, "Other/x.txt"); got != "" {
		t.Errorf("mountLocal outside the root = %q, want \"\"", got)
	}
	if got := mountPath(nil, abs); got != "Survey-North.tif" {
		t.Errorf("mountPath with no mount = %q, want the base name", got)
	}
}

// The filter's stream lists its file until it is closed.
func TestHydrateReadCloserEndsItsJob(t *testing.T) {
	var h hydrateJobs
	changed := 0
	r := &hydrateReadCloser{
		ReadCloser: &fakeReadCloser{data: []byte("hello"), err: io.EOF},
		remotePath: "a.bin", report: func(string, string, error) {},
		job: h.begin("acct", "a.bin", 5), changed: func() { changed++ },
	}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	if s := h.snapshot(); len(s) != 1 || s[0].Sent != 5 {
		t.Fatalf("snapshot while open = %+v", s)
	}
	_ = r.Close()
	if s := h.snapshot(); len(s) != 0 || changed != 1 {
		t.Errorf("after Close: %d jobs, changed %d times", len(s), changed)
	}
}
