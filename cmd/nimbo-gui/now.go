package main

import (
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/brand"
)

// "What is happening right now" (Deck #988): one status line for the flyout
// and the tray, and the list behind it for the Status window's Activity tab.
// App.NowStatus gathers every account's engine and on-demand watchers into a
// nowInput; composeNow (pure) picks the line. The original bug it must never
// recreate: a 55-minute recheck of every folder while the flyout said
// "Up to date".

// NowJobDTO is one transfer in flight (or queued), for "Right now".
type NowJobDTO struct {
	Account string `json:"account"` // login; "" with a single account
	Kind    string `json:"kind"`    // "upload" | "download"
	Path    string `json:"path"`    // account-relative, slash-separated
	Size    int64  `json:"size"`    // 0 = unknown
	Sent    int64  `json:"sent"`
	Large   bool   `json:"large"`
	State   string `json:"state"` // "running" | "waiting" | "paused" | "setaside"
}

// NowCheckDTO is a folder check (scan or reconcile pass) in progress.
type NowCheckDTO struct {
	Account string `json:"account"`
	Folder  string `json:"folder"` // the folder being listed now, "" when unknown
	Dirs    int    `json:"dirs"`   // folders listed so far
}

// NowStatusDTO is the composed status line and the detail behind it.
type NowStatusDTO struct {
	State     string        `json:"state"` // "attention" | "paused" | "offline" | "busy" | "idle"
	Main      string        `json:"main"`
	Sub       string        `json:"sub"`
	Progress  float64       `json:"progress"` // 0..1, -1 = none
	Tab       string        `json:"tab"`      // Status tab a click opens
	Jobs      []NowJobDTO   `json:"jobs"`
	Checking  []NowCheckDTO `json:"checking"`
	LastCheck string        `json:"lastCheck"` // RFC3339, "" = none yet
}

// nowInput is everything composeNow needs, gathered by App.NowStatus.
type nowInput struct {
	ShownAccount     string // the account the flyout shows (as in Jobs/Checking Account)
	Paused           bool
	PauseUntil       string // "18:00" for a timed pause, else ""
	Offline          bool
	Waiting          int // changes waiting to go up (0 = unknown or none)
	Attention        AttentionInfo
	FirstSyncWaiting bool // a first sync is running and no folder was chosen to come first
	Jobs             []NowJobDTO
	Checking         []NowCheckDTO
	LastCheck        time.Time
	CheckIncomplete  bool
	InUse            int
	LargeRunning     int
	LargeAside       int
	OtherBusy        string // another account's name when it is busy/offline
	Now              time.Time

	// Beyond the brief's fields, so the line never claims "Up to date" when
	// the engine's own status says otherwise:
	// Problem is the shown engine's status when sync has stopped or failed
	// ("Error", "Sync stopped — local folder missing", "Sign in again").
	Problem string
	// Working is the shown engine's status while it works on something no
	// job or check explains (deletes, moves, "Moving your folder…").
	Working string
	// Holding is the engine's "Waiting — Report.docx is open in another
	// program" line: nothing is moving, but not everything is up yet.
	Holding string
	// OtherOffline: OtherBusy can't reach its server (rather than syncing);
	// OtherProblem: its sync stopped or failed.
	OtherOffline bool
	OtherProblem bool
}

// composeNow picks the status line: needs you, then paused, then offline,
// then busy, then idle. It is pure, so every case is a table row.
func composeNow(in nowInput, brandName string) NowStatusDTO {
	_ = brandName // no line names the app today; kept so one that does stays white-label
	out := NowStatusDTO{Progress: -1, Tab: "activity", Jobs: in.Jobs, Checking: in.Checking}
	if out.Jobs == nil {
		out.Jobs = []NowJobDTO{}
	}
	if out.Checking == nil {
		out.Checking = []NowCheckDTO{}
	}
	if !in.LastCheck.IsZero() {
		out.LastCheck = in.LastCheck.Format(time.RFC3339)
	}

	// Needs you: every account counts (as the tray warning always has).
	at := in.Attention
	if n := at.Conflicts + at.Blocked + at.Detached; n > 0 {
		out.State = "attention"
		out.Main = plural(n, "item needs you", "items need you")
		var parts []string
		if at.Blocked > 0 {
			parts = append(parts, thousands(at.Blocked)+" can't sync")
		}
		if at.Conflicts > 0 {
			parts = append(parts, plural(at.Conflicts, "conflict", "conflicts"))
		}
		if at.Detached > 0 {
			parts = append(parts, plural(at.Detached, "folder no longer shared", "folders no longer shared"))
		}
		out.Sub = strings.Join(parts, ", ")
		// The tab the flyout's "Review →" opened: the most urgent kind first.
		switch {
		case at.Conflicts > 0:
			out.Tab = "conflicts"
		case at.Blocked > 0:
			out.Tab = "blocked"
		default:
			out.Tab = "detached"
		}
		return out
	}

	if in.Paused {
		out.State = "paused"
		out.Main = "Paused"
		var parts []string
		if in.PauseUntil != "" {
			parts = append(parts, "until "+in.PauseUntil)
		}
		if in.Waiting > 0 {
			parts = append(parts, plural(in.Waiting, "change waiting", "changes waiting"))
		}
		out.Sub = strings.Join(parts, " · ")
		return out
	}

	if in.Offline || in.Problem != "" {
		out.State = "offline"
		if in.Offline {
			out.Main = "Can't reach the server"
		} else {
			out.Main = in.Problem
		}
		if in.Waiting > 0 {
			out.Sub = plural(in.Waiting, "change waiting to upload", "changes waiting to upload")
		}
		return out
	}

	// The shown account's work only; other accounts get a mention below.
	var running []NowJobDTO
	ups, downs := 0, 0
	for _, j := range in.Jobs {
		if j.Account != in.ShownAccount || j.State != "running" {
			continue
		}
		running = append(running, j)
		if j.Large {
			continue // counted as "large files syncing"
		}
		if j.Kind == "upload" {
			ups++
		} else {
			downs++
		}
	}
	dirs, checking := 0, false
	for _, c := range in.Checking {
		if c.Account == in.ShownAccount {
			checking = true
			dirs += c.Dirs
		}
	}

	var extras []string
	if in.FirstSyncWaiting {
		extras = append(extras, "Choose folders to download first")
		out.Tab = "first"
	}
	if in.InUse > 0 {
		extras = append(extras, plural(in.InUse, "file in use", "files in use"))
	}
	if in.LargeRunning > 0 {
		extras = append(extras, plural(in.LargeRunning, "large file syncing", "large files syncing"))
	}
	if in.LargeAside > 0 {
		extras = append(extras, plural(in.LargeAside, "large file set aside", "large files set aside"))
	}

	if len(running) > 0 || checking || in.Working != "" {
		out.State = "busy"
		var parts []string
		if s := transferSummary(ups, downs); s != "" {
			parts = append(parts, s)
		}
		if j, ok := mainJob(running); ok {
			verb := "Downloading "
			if j.Kind == "upload" {
				verb = "Uploading "
			}
			out.Main = verb + path.Base(j.Path)
			if j.Size > 0 {
				out.Progress = clamp01(float64(j.Sent) / float64(j.Size))
			}
			if checking {
				parts = append(parts, "checking folders")
			}
		} else if checking {
			out.Main = "Checking folders…"
			if dirs > 0 {
				out.Main += " " + thousands(dirs)
			}
		} else {
			out.Main = in.Working
		}
		out.Sub = strings.Join(append(parts, extras...), " · ")
		return out
	}

	out.State = "idle"
	out.Main = "Up to date"
	if in.Holding != "" {
		out.Main = in.Holding
	}
	var parts []string
	switch {
	case in.CheckIncomplete:
		parts = append(parts, "Some folders couldn't be checked")
	case !in.LastCheck.IsZero():
		parts = append(parts, "Checked "+checkedAgo(in.Now.Sub(in.LastCheck)))
	}
	parts = append(parts, extras...)
	if in.OtherBusy != "" {
		switch {
		case in.OtherOffline:
			parts = append(parts, in.OtherBusy+" account can't reach its server")
		case in.OtherProblem:
			parts = append(parts, in.OtherBusy+" account needs attention")
		default:
			parts = append(parts, in.OtherBusy+" account syncing")
		}
	}
	out.Sub = strings.Join(parts, " · ")
	return out
}

// mainJob is the transfer the main line names: a user-visible (small) one
// before a large-file lane job, each in list order.
func mainJob(running []NowJobDTO) (NowJobDTO, bool) {
	for _, j := range running {
		if !j.Large {
			return j, true
		}
	}
	if len(running) > 0 {
		return running[0], true
	}
	return NowJobDTO{}, false
}

// transferSummary is "2 uploads, 1 download" ("" for none).
func transferSummary(ups, downs int) string {
	var parts []string
	if ups > 0 {
		parts = append(parts, plural(ups, "upload", "uploads"))
	}
	if downs > 0 {
		parts = append(parts, plural(downs, "download", "downloads"))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return thousands(n) + " " + many
}

// checkedAgo is "just now", "1 min ago", "3 hours ago", "2 days ago".
func checkedAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "min ago", "min ago")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour ago", "hours ago")
	default:
		return plural(int(d/(24*time.Hour)), "day ago", "days ago")
	}
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	}
	return f
}

// hydrateJobs tracks on-demand downloads streamed through the cfapi filter
// (a file opened in Explorer or an app). The filter asks for one file in
// several ranges at once; ranges of the same file are one job.
type hydrateJobs struct {
	mu   sync.Mutex
	jobs map[string]*hydrateEntry // account + "\x00" + remote path
	seq  uint64
}

type hydrateEntry struct {
	account, remotePath string
	size, got           int64
	refs                int
	seq                 uint64
}

// hjob is one range request's handle on its file's job.
type hjob struct {
	h     *hydrateJobs
	key   string
	e     *hydrateEntry
	ended bool
}

// begin registers a range request. size is how far into the file the request
// reaches (offset + length; 0 = unknown); the job's size is the furthest any
// of its ranges reaches.
func (h *hydrateJobs) begin(account, remotePath string, size int64) *hjob {
	key := account + "\x00" + remotePath
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.jobs == nil {
		h.jobs = map[string]*hydrateEntry{}
	}
	e := h.jobs[key]
	if e == nil {
		h.seq++
		e = &hydrateEntry{account: account, remotePath: remotePath, seq: h.seq}
		h.jobs[key] = e
	}
	e.refs++
	if size > e.size {
		e.size = size
	}
	return &hjob{h: h, key: key, e: e}
}

// add counts n more bytes received. Safe on a nil job.
func (j *hjob) add(n int64) {
	if j == nil || n <= 0 {
		return
	}
	j.h.mu.Lock()
	j.e.got += n
	j.h.mu.Unlock()
}

// end closes this range; the job goes when its last range does. Safe on a
// nil job and safe to call twice.
func (j *hjob) end() {
	if j == nil {
		return
	}
	j.h.mu.Lock()
	defer j.h.mu.Unlock()
	if j.ended {
		return
	}
	j.ended = true
	j.e.refs--
	if j.e.refs <= 0 && j.h.jobs[j.key] == j.e {
		delete(j.h.jobs, j.key)
	}
}

// snapshot lists the jobs in start order. Account is as passed to begin and
// Path the raw remote path; NowStatus maps both for display.
func (h *hydrateJobs) snapshot() []NowJobDTO {
	h.mu.Lock()
	es := make([]*hydrateEntry, 0, len(h.jobs))
	for _, e := range h.jobs {
		es = append(es, e)
	}
	h.mu.Unlock()
	sort.Slice(es, func(a, b int) bool { return es[a].seq < es[b].seq })
	out := make([]NowJobDTO, 0, len(es))
	h.mu.Lock()
	for _, e := range es {
		out = append(out, NowJobDTO{Account: e.account, Kind: "download", Path: e.remotePath,
			Size: e.size, Sent: e.got, State: "running"})
	}
	h.mu.Unlock()
	return out
}

// nowEmitter turns a stream of "something changed" nudges into at most one
// emit per minGap, always with one emit after the last nudge. nudge never
// blocks beyond a short mutex hold: it is called on transfer and watcher
// goroutines (vfs Ops.Changed is synchronous), and emit runs on a timer
// goroutine, never the caller's.
type nowEmitter struct {
	minGap  time.Duration
	emit    func()
	mu      sync.Mutex
	pending bool
	last    time.Time
}

func newNowEmitter(minGap time.Duration, emit func()) *nowEmitter {
	return &nowEmitter{minGap: minGap, emit: emit}
}

func (e *nowEmitter) nudge() {
	e.mu.Lock()
	if e.pending {
		e.mu.Unlock()
		return // the scheduled emit comes after this nudge anyway
	}
	e.pending = true
	wait := time.Until(e.last.Add(e.minGap))
	if wait < 0 {
		wait = 0
	}
	e.mu.Unlock()
	time.AfterFunc(wait, e.fire)
}

func (e *nowEmitter) fire() {
	e.mu.Lock()
	e.pending = false
	e.last = time.Now()
	e.mu.Unlock()
	e.emit()
}

// nudgeNow tells the windows the status may have changed ("now" event, at most
// twice a second). Cheap and non-blocking; safe from any goroutine.
func (a *App) nudgeNow() {
	a.nowEmOnce.Do(func() {
		a.nowEm = newNowEmitter(500*time.Millisecond, func() { a.emit("now") })
	})
	a.nowEm.nudge()
}

// checkQuietPeriod is how long a live scan runs before the line shows it, as
// the engine's own scan narration waits (agent scanQuietPeriod).
const checkQuietPeriod = 1500 * time.Millisecond

// NowStatus is the flyout's status line and the Status window's "Right now".
// The windows call it on every "now" event, so the attention counts (which
// read files) come from a cache at most two seconds old.
func (a *App) NowStatus() NowStatusDTO {
	return a.nowStatus(a.cachedAttention())
}

// cachedAttention is Attention, reused for up to two seconds. Safe from any
// goroutine (Wails calls and the tray).
func (a *App) cachedAttention() AttentionInfo {
	a.attnMu.Lock()
	at, fresh, gen := a.attn, !a.attnAt.IsZero() && time.Since(a.attnAt) <= 2*time.Second, a.attnGen
	a.attnMu.Unlock()
	if fresh {
		return at
	}
	at = a.Attention() // engines are called outside attnMu
	a.attnMu.Lock()
	if a.attnGen == gen { // not invalidated meanwhile: the counts may be stale
		a.attn, a.attnAt = at, time.Now()
	}
	a.attnMu.Unlock()
	return at
}

// attentionChanged drops the cached counts (a conflict, blocked file, lock or
// detached folder came or went) and refreshes the status line.
func (a *App) attentionChanged() {
	a.attnMu.Lock()
	a.attnAt = time.Time{}
	a.attnGen++
	a.attnMu.Unlock()
	a.nudgeNow()
}

// nowStatus composes the status with the attention counts given (the tray
// passes its cached ones; Attention reads files).
func (a *App) nowStatus(att AttentionInfo) NowStatusDTO {
	now := time.Now()
	eng := a.eng
	if eng == nil {
		st := a.Status()
		state := "idle"
		if strings.HasPrefix(st, "Can't reach") {
			state = "offline"
		}
		return NowStatusDTO{State: state, Main: st, Progress: -1, Tab: "activity",
			Jobs: []NowJobDTO{}, Checking: []NowCheckDTO{}}
	}
	in := a.gatherNow(eng, att, now)
	out := composeNow(in, brand.Current.Name)
	if a.secondaryCount() == 0 {
		// One account: no account column (as LaneList).
		for i := range out.Jobs {
			out.Jobs[i].Account = ""
		}
		for i := range out.Checking {
			out.Checking[i].Account = ""
		}
	}
	return out
}

// nowEngine is one account's engine with what NowStatus needs to know of it.
type nowEngine struct {
	eng    *agent.Engine
	login  string
	shown  bool
	status string
	mounts map[string]*odMount // this account's on-demand mounts by local dir
	down   bool                // one of its mounts can't list the server's root
}

// gatherNow collects every account's jobs and checks, deduped by local path,
// plus the shown account's own state. It never holds a lock of its own while
// calling an engine or a watcher.
func (a *App) gatherNow(shown *agent.Engine, att AttentionInfo, now time.Time) nowInput {
	engines := []*nowEngine{{eng: shown, login: shown.Account.LoginName, shown: true, status: a.status}}
	secs := a.secs()
	ids := make([]string, 0, len(secs))
	for id := range secs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	a.acctMu.Lock()
	for _, id := range ids {
		if se := secs[id]; se != nil && se.eng != nil {
			engines = append(engines, &nowEngine{eng: se.eng, login: se.eng.Account.LoginName, status: a.acctStatus[id]})
		}
	}
	a.acctMu.Unlock()
	byID := map[string]*nowEngine{}
	for _, ne := range engines {
		ne.mounts = map[string]*odMount{}
		byID[ne.eng.Account.ID] = ne
	}
	for dir, m := range a.mounts() {
		if ne := byID[m.accountID]; ne != nil {
			ne.mounts[dir] = m
		}
	}

	in := nowInput{ShownAccount: engines[0].login, Attention: att, InUse: att.Locked, Now: now}
	seen := map[string]bool{}
	add := func(abs string, j NowJobDTO) {
		if abs != "" {
			k := pathKey(abs)
			if seen[k] {
				return
			}
			seen[k] = true
		}
		in.Jobs = append(in.Jobs, j)
	}
	hydr := a.hjobs.snapshot()

	for _, ne := range engines {
		var pairs map[string]string // local dir -> remote root, read once when needed
		pairRoot := func(dir string) string {
			if pairs == nil {
				pairs = map[string]string{}
				if ps, err := ne.eng.Pairs(); err == nil {
					for _, p := range ps {
						pairs[filepath.Clean(p.LocalDir)] = strings.Trim(p.RemoteRoot, "/")
					}
				}
			}
			return pairs[filepath.Clean(dir)]
		}
		for _, j := range ne.eng.NowJobs() {
			abs, acctPath := "", ""
			if j.LocalDir != "" {
				abs = filepath.Join(j.LocalDir, filepath.FromSlash(j.Path))
				acctPath = joinRemote(pairRoot(j.LocalDir), j.Path)
			} else {
				abs = j.Path
				acctPath = mountPath(ne.mounts, abs)
			}
			add(abs, NowJobDTO{Account: ne.login, Kind: j.Kind, Path: acctPath, Size: j.Size, Sent: j.Sent, Large: j.Large, State: j.State})
		}
		// Files opened through the filter (registered by account ID, raw path).
		for _, h := range hydr {
			if h.Account != ne.eng.Account.ID {
				continue
			}
			shownPath := vfsDisplayPath(ne.eng.Escaper(), h.Path)
			h.Account, h.Path = ne.login, shownPath
			add(mountLocal(ne.mounts, shownPath), h)
		}
		var incomplete, haveCheck bool
		var oldest time.Time // the least recent mount's last full check (zero = one never finished)
		for _, m := range ne.mounts {
			w := a.watcherOf(m)
			if w == nil {
				continue
			}
			wn := w.Now()
			ne.down = ne.down || wn.ServerDown
			for _, p := range wn.Uploading {
				add(p, NowJobDTO{Account: ne.login, Kind: "upload", Path: mountPath(ne.mounts, p), State: "running"})
			}
			for _, p := range wn.Hydrating {
				add(p, NowJobDTO{Account: ne.login, Kind: "download", Path: mountPath(ne.mounts, p), State: "running"})
			}
			if wn.Checking {
				in.Checking = append(in.Checking, NowCheckDTO{Account: ne.login, Folder: wn.Folder, Dirs: wn.DirsChecked})
			}
			if ne.shown {
				incomplete = incomplete || wn.CheckIncomplete
				if !haveCheck || wn.LastFullCheck.Before(oldest) {
					oldest = wn.LastFullCheck
				}
				haveCheck = true
			}
		}
		if sn := ne.eng.ScanNow(); showScan(sn, now) {
			c := NowCheckDTO{Account: ne.login, Folder: sn.Folder}
			if sn.Phase == "server" {
				c.Dirs = sn.Count // the other stages count files, not folders
			}
			in.Checking = append(in.Checking, c)
		}
		if ne.shown {
			if len(ne.mounts) > 0 {
				// On-demand: the watchers' passes are the checks.
				in.LastCheck, in.CheckIncomplete = oldest, incomplete
			} else {
				in.LastCheck, in.CheckIncomplete = ne.eng.LastFullCheck()
			}
		}
	}

	for _, j := range in.Jobs {
		if j.Account != in.ShownAccount {
			continue
		}
		switch {
		case j.Large && j.State == "running":
			in.LargeRunning++
		case j.Large && j.State == "setaside":
			in.LargeAside++
		}
		if j.Kind == "upload" && (j.State == "waiting" || j.State == "paused") {
			in.Waiting++
		}
	}

	ps := shown.PauseState()
	in.Paused, in.PauseUntil = ps.Paused, ps.Until
	in.Offline, in.Problem, in.Working, in.Holding = accountState(engines[0].status, engines[0].down)
	if p := shown.Progress(); p.Active && len(p.Clones) > 0 {
		chosen := 0
		for _, c := range p.Clones {
			chosen += len(c.First)
		}
		in.FirstSyncWaiting = chosen == 0
	}

	// Another account working while this one rests: say so on the second line.
	for _, ne := range engines[1:] {
		off, prob, working, _ := accountState(ne.status, ne.down)
		busy := working != ""
		for _, j := range in.Jobs {
			busy = busy || (j.Account == ne.login && j.State == "running")
		}
		for _, c := range in.Checking {
			busy = busy || c.Account == ne.login
		}
		if busy || off || prob != "" {
			in.OtherBusy = ne.login
			in.OtherProblem = !busy && prob != "" && !off
			in.OtherOffline = !busy && off
			break
		}
	}
	return in
}

// classifyStatus reads an engine status line: the server unreachable, sync
// stopped or failed (the line itself), working on something (the line), or
// holding uploads back (the line). It matches the exact lines their producers
// set (internal/agent: status(), heldStatus, waitingStatus,
// heldDamagedStatus, the damage guard; the GUI's own start-up lines), never
// words anywhere in them: the "Waiting —" lines carry file names, and
// "error-log.xlsx is open in another program" is not an error.
func classifyStatus(s string) (offline bool, problem, working, holding string) {
	switch {
	case strings.HasPrefix(s, "Waiting —"):
		return false, "", "", s
	case s == "Offline" || strings.HasPrefix(s, "Can't reach your server"):
		return true, "", "", ""
	case s == "Error" || s == "Sign in again" || strings.HasPrefix(s, "Sync stopped —") ||
		strings.HasPrefix(s, "Sync paused —") || strings.HasPrefix(s, "Not running:"):
		return false, s, "", ""
	case isBusyStatus(s):
		return false, "", s, ""
	}
	return false, "", "", ""
}

// accountState is classifyStatus for one account, with its on-demand
// watchers' outage: on-demand mode runs no sync pass, so its engine never
// says "Offline", but a failed root listing (down) does.
func accountState(status string, down bool) (offline bool, problem, working, holding string) {
	offline, problem, working, holding = classifyStatus(status)
	if down && !offline {
		return true, "", "", ""
	}
	return offline, problem, working, holding
}

// showScan reports whether a live scan shows as "Checking folders…": only
// once it has run past the quiet period. A warm pass (every 15 s without
// notify_push) is over in milliseconds, and the line would flicker.
func showScan(sn agent.ScanNow, now time.Time) bool {
	return sn.Active && now.Sub(sn.Since) >= checkQuietPeriod
}

// pathKey is a local path's identity for deduping: case-insensitive on Windows.
func pathKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func joinRemote(root, rel string) string {
	root = strings.Trim(root, "/")
	switch {
	case root == "":
		return rel
	case rel == "" || rel == ".":
		return root
	}
	return root + "/" + rel
}

// mountPath maps an absolute local path to its account-relative path through
// the mounts containing it; with none it is the base name.
func mountPath(mounts map[string]*odMount, abs string) string {
	c := filepath.Clean(abs)
	for dir, m := range mounts {
		rel, err := filepath.Rel(filepath.Clean(dir), c)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		if rel == "." {
			rel = ""
		}
		return joinRemote(m.remoteRoot, rel)
	}
	return filepath.Base(c)
}

// mountLocal is mountPath backwards: the local path of an account-relative
// one, or "" when no mount holds it.
func mountLocal(mounts map[string]*odMount, acctPath string) string {
	acctPath = strings.Trim(acctPath, "/")
	for dir, m := range mounts {
		root := strings.Trim(m.remoteRoot, "/")
		rel := acctPath
		if root != "" {
			if !strings.HasPrefix(acctPath, root+"/") {
				continue
			}
			rel = acctPath[len(root)+1:]
		}
		return filepath.Join(dir, filepath.FromSlash(rel))
	}
	return ""
}

// trayNow is the composed status for the tray, refreshed at most twice a
// second. Only animateTray's goroutine calls it.
func (a *App) trayNow() NowStatusDTO {
	if a.trayNowAt.IsZero() || time.Since(a.trayNowAt) > 500*time.Millisecond {
		a.trayNowDTO = a.nowStatus(a.cachedAttention())
		a.trayNowAt = time.Now()
	}
	return a.trayNowDTO
}
