package main

import (
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/agent"
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
		{name: "uploads waiting, nothing moving",
			in:    nowInput{Waiting: 2, LastCheck: now.Add(-time.Minute)},
			state: "idle", main: "2 changes waiting to upload", sub: "Checked 1 min ago", tab: "activity"},
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
		{"Scanning…", "busy", "Scanning…"},
		{"Moving your folder…", "busy", "Moving your folder…"},
		{"Waiting — Report.docx is open in another program", "idle", "Waiting — Report.docx is open in another program"},
		{"Up to date", "idle", "Up to date"},
		{"Sync paused — needs review", "offline", "Sync paused — needs review"},
		{"Sync stopped — too many deletions", "offline", "Sync stopped — too many deletions"},
		// File names in a "Waiting" line never make it a problem.
		{"Waiting — error-log.xlsx is open in another program", "idle", "Waiting — error-log.xlsx is open in another program"},
		{"Waiting — stopped-clock.png is in use by someone else", "idle", "Waiting — stopped-clock.png is in use by someone else"},
		{"Waiting — how to sign in.docx: the copy on the server is damaged", "idle", "Waiting — how to sign in.docx: the copy on the server is damaged"},
		{"Waiting — Sync notes.txt is open in another program", "idle", "Waiting — Sync notes.txt is open in another program"},
	} {
		in := nowInput{Now: time.Now()}
		in.Offline, in.Problem, in.Working, in.Holding = classifyStatus(tc.status)
		// Set long enough ago to be past the quiet period (see
		// TestQuietWorkingHoldsRoutinePassLines for a fresh one).
		in.Working, _ = quietWorking(in.Working, in.Now.Add(-time.Minute), in.Now)
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
	// Bytes arriving refresh the line (the progress would freeze otherwise).
	read := changed
	if read == 0 {
		t.Error("reading bytes never told anyone")
	}
	_ = r.Close()
	if s := h.snapshot(); len(s) != 0 || changed != read+1 {
		t.Errorf("after Close: %d jobs, changed %d times, want %d", len(s), changed, read+1)
	}
}

// Another account whose sync stopped is not "can't reach its server".
func TestComposeNowOtherAccountStates(t *testing.T) {
	for _, tc := range []struct {
		in   nowInput
		want string
	}{
		{nowInput{OtherBusy: "Work"}, "Work account syncing"},
		{nowInput{OtherBusy: "Work", OtherOffline: true}, "Work account can't reach its server"},
		{nowInput{OtherBusy: "Work", OtherProblem: true}, "Work account needs attention"},
	} {
		tc.in.Now = time.Now()
		if got := composeNow(tc.in, "Nimbo"); got.Sub != tc.want {
			t.Errorf("sub = %q, want %q", got.Sub, tc.want)
		}
	}
}

// On-demand mode: a watcher that can't list the server's root makes the
// account offline whatever its engine's status says (Deck #988 review).
func TestAccountStateServerDown(t *testing.T) {
	in := nowInput{Now: time.Now(), LastCheck: time.Now().Add(-time.Hour), CheckIncomplete: true}
	in.Offline, in.Problem, in.Working, in.Holding = accountState("Up to date", true)
	if got := composeNow(in, "Nimbo"); got.State != "offline" || got.Main != "Can't reach the server" {
		t.Errorf("server down: got %q / %q", got.State, got.Main)
	}
	if off, prob, _, _ := accountState("Up to date", false); off || prob != "" {
		t.Errorf("server up: offline=%v problem=%q", off, prob)
	}
	if off, _, _, _ := accountState("Offline", false); !off {
		t.Error("an engine saying Offline is offline")
	}
}

// A live scan shows only once it has run past the quiet period.
func TestShowScanWaitsOutTheQuietPeriod(t *testing.T) {
	now := time.Now()
	if showScan(agent.ScanNow{Active: true, Since: now.Add(-100 * time.Millisecond)}, now) {
		t.Error("a scan 100 ms old is shown")
	}
	if !showScan(agent.ScanNow{Active: true, Since: now.Add(-2 * time.Second)}, now) {
		t.Error("a scan 2 s old is not shown")
	}
	if showScan(agent.ScanNow{Since: now.Add(-time.Hour)}, now) {
		t.Error("an inactive scan is shown")
	}
}

// The tray reads the mount and engine maps twice a second while mounts come
// and go on other goroutines; every access goes through mapsMu (run with
// -race).
func TestMountAndSecondaryMapsAreSafeToReadWhileWritten(t *testing.T) {
	a := &App{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			dir := filepath.Join("x", strconv.Itoa(i%7))
			m := &odMount{accountID: "a"}
			a.addMount(dir, m)
			a.setWatcher(m, nil)
			a.addSecondary(strconv.Itoa(i%5), &secondaryEngine{})
			a.dropMount(dir)
			a.dropSecondary(strconv.Itoa(i % 3))
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		for _, m := range a.mounts() {
			_ = a.watcherOf(m)
		}
		_ = len(a.secs()) + a.secondaryCount()
		_ = a.mount("x") != nil || a.secondary("1") != nil || a.mountsReady()
	}
}

// "Scanning…" and "Syncing…" are set at the start of every poll pass, most of
// them over in milliseconds: they count as work only once they outlast the
// quiet period. Other working lines show at once (Deck #988 review).
func TestQuietWorkingHoldsRoutinePassLines(t *testing.T) {
	now := time.Now()
	for _, s := range []string{"Scanning…", "Syncing…"} {
		w, wait := quietWorking(s, now.Add(-100*time.Millisecond), now)
		if w != "" || wait != checkQuietPeriod-100*time.Millisecond {
			t.Errorf("%q 100 ms old: working %q, wait %v", s, w, wait)
		}
		in := nowInput{Now: now, LastCheck: now.Add(-time.Minute), Working: w}
		if got := composeNow(in, "Nimbo"); got.State != "idle" || got.Main != "Up to date" {
			t.Errorf("%q 100 ms old: got %q / %q, want the idle line", s, got.State, got.Main)
		}
		if w, wait := quietWorking(s, now.Add(-2*time.Second), now); w != s || wait != 0 {
			t.Errorf("%q 2 s old: working %q, wait %v", s, w, wait)
		}
	}
	if w, wait := quietWorking("Moving your folder…", now, now); w != "Moving your folder…" || wait != 0 {
		t.Errorf("Moving your folder… just set: working %q, wait %v, want it at once", w, wait)
	}
	if w, wait := quietWorking("", now, now); w != "" || wait != 0 {
		t.Errorf("no line: working %q, wait %v", w, wait)
	}
}

// The status text's time moves only when the text changes, so a pass that
// re-sets "Syncing…" doesn't restart the quiet period.
func TestStatusLineSinceMovesOnlyOnChange(t *testing.T) {
	var l statusLine
	t0 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	l.set("Syncing…", t0)
	l.set("Syncing…", t0.Add(time.Second))
	if s, since := l.get(); s != "Syncing…" || !since.Equal(t0) {
		t.Errorf("same text again: %q since %v, want since %v", s, since, t0)
	}
	l.set("Up to date", t0.Add(2*time.Second))
	if s, since := l.get(); s != "Up to date" || !since.Equal(t0.Add(2*time.Second)) {
		t.Errorf("new text: %q since %v", s, since)
	}
}

// setStatus runs on engine goroutines while the tray and Wails calls read
// the status (run with -race).
func TestStatusIsSafeToReadWhileSet(t *testing.T) {
	a := &App{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			a.setStatus("Syncing…")
			a.setStatus("Up to date")
		}
	}()
	for {
		select {
		case <-done:
			if got := a.Status(); got != "Up to date" {
				t.Errorf("Status = %q", got)
			}
			return
		default:
		}
		_ = a.Status()
		_, _ = a.status.get()
	}
}

// An on-demand pass (like a live scan) shows only past the quiet period, and
// says when it would.
func TestCheckShownWaitsOutTheQuietPeriod(t *testing.T) {
	now := time.Now()
	if ok, wait := checkShown(true, now.Add(-time.Second), now); ok || wait != checkQuietPeriod-time.Second {
		t.Errorf("1 s old: shown %v, wait %v", ok, wait)
	}
	if ok, wait := checkShown(true, now.Add(-2*time.Second), now); !ok || wait != 0 {
		t.Errorf("2 s old: shown %v, wait %v", ok, wait)
	}
	if ok, wait := checkShown(false, now, now); ok || wait != 0 {
		t.Errorf("inactive: shown %v, wait %v", ok, wait)
	}
	var d time.Duration
	soonest(&d, 0)
	soonest(&d, time.Second)
	soonest(&d, 300*time.Millisecond)
	soonest(&d, 2*time.Second)
	if d != 300*time.Millisecond {
		t.Errorf("soonest = %v, want 300ms", d)
	}
}

// A held-back check schedules one look at the moment it would show; a later
// request while an earlier one is due adds no timer.
func TestNudgeAfterKeepsTheEarliest(t *testing.T) {
	a := &App{}
	a.nudgeAfter(time.Hour)
	first := a.revealAt
	a.nudgeAfter(2 * time.Hour)
	if !a.revealAt.Equal(first) {
		t.Errorf("a later request moved the pending look from %v to %v", first, a.revealAt)
	}
	a.nudgeAfter(time.Millisecond)
	if !a.revealAt.Before(first) {
		t.Errorf("an earlier request did not bring the look forward: %v", a.revealAt)
	}
}

// Changes queued to go up are not "Up to date" (Deck #988 review: on-demand
// uploads waiting out a debounce or a retry read as up to date).
func TestComposeNowWaitingIsNotUpToDate(t *testing.T) {
	now := time.Now()
	got := composeNow(nowInput{Waiting: 2, LastCheck: now.Add(-time.Minute), Now: now}, "Nimbo")
	if got.State != "idle" || got.Main != "2 changes waiting to upload" || got.Sub != "Checked 1 min ago" {
		t.Errorf("2 waiting: got %q / %q / %q", got.State, got.Main, got.Sub)
	}
	// Only waiting (a retry backing off for up to 30 min): the tray stays still.
	if ts := trayStateFor(got.State); ts != "idle" {
		t.Errorf("2 waiting: tray %q, want idle", ts)
	}
	got = composeNow(nowInput{Waiting: 2, CheckIncomplete: true, InUse: 1, Now: now}, "Nimbo")
	if got.Main != "2 changes waiting to upload" || got.Sub != "Some folders couldn't be checked · 1 file in use" {
		t.Errorf("2 waiting, incomplete: got %q / %q", got.Main, got.Sub)
	}
	// A check showing makes it busy as before.
	got = composeNow(nowInput{Waiting: 2, Checking: []NowCheckDTO{{Dirs: 3}}, Now: now}, "Nimbo")
	if got.State != "busy" || got.Main != "Checking folders… 3" {
		t.Errorf("waiting while checking: got %q / %q", got.State, got.Main)
	}
	got = composeNow(nowInput{Waiting: 1, Now: now}, "Nimbo")
	if got.Main != "1 change waiting to upload" {
		t.Errorf("1 waiting: main %q", got.Main)
	}
	// A file held open by a program says why instead.
	hold := "Waiting — Report.docx is open in another program"
	got = composeNow(nowInput{Waiting: 1, Holding: hold, Now: now}, "Nimbo")
	if got.State != "idle" || got.Main != hold {
		t.Errorf("held: got %q / %q", got.State, got.Main)
	}
	// Running work still names the transfer.
	got = composeNow(nowInput{Waiting: 3, Now: now,
		Jobs: []NowJobDTO{{Kind: "upload", Path: "a.txt", State: "running"}}}, "Nimbo")
	if got.State != "busy" || got.Main != "Uploading a.txt" {
		t.Errorf("running and waiting: main %q", got.Main)
	}
}

// Two accounts with the same login on different servers stay apart: the
// shown account's line is matched on the account ID, not the name.
func TestComposeNowMatchesAccountsByID(t *testing.T) {
	now := time.Now()
	other := NowJobDTO{Account: "adam", AccountID: "id-work", Kind: "upload", Path: "x.txt", State: "running"}
	got := composeNow(nowInput{ShownAccount: "id-home", Jobs: []NowJobDTO{other},
		Checking: []NowCheckDTO{{Account: "adam", AccountID: "id-work", Dirs: 5}}, Now: now}, "Nimbo")
	if got.State != "idle" {
		t.Errorf("the other server's adam drove the line: %q / %q", got.State, got.Main)
	}
	mine := other
	mine.AccountID = "id-home"
	got = composeNow(nowInput{ShownAccount: "id-home", Jobs: []NowJobDTO{mine}, Now: now}, "Nimbo")
	if got.State != "busy" || got.Main != "Uploading x.txt" {
		t.Errorf("own job: got %q / %q", got.State, got.Main)
	}
}
