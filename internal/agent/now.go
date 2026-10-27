package agent

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

// Live snapshots for the flyout's "what is Nimbo doing right now" line: the
// small transfers in flight, the scan's stage and count, and when the last
// complete check finished. Large transfers are the lane's (LaneEntries); the
// snapshot merges both so a reader sees one list.

// NowJob is one transfer, running or queued.
type NowJob struct {
	Kind     string // "upload" | "download"
	LocalDir string // the sync folder it belongs to ("" when unknown)
	Path     string // pair-relative, slash-separated (base name when unknown)
	Size     int64
	Sent     int64
	Large    bool   // a lane (long-transfer) job
	State    string // "running" | "waiting" | "paused" | "setaside"
}

// ScanNow is the scan's current stage. Folder is "" where unknown.
type ScanNow struct {
	Active bool
	Phase  string // "server" | "local" | "compare" | "moves"
	Count  int
	Folder string
}

type nowJob struct {
	NowJob
	seq uint64 // start order, so the list is stable
}

// nowState is guarded by its own mutex; maps are created on first use so a
// zero-value Engine (tests, pre-Run) works.
type nowState struct {
	mu      sync.Mutex
	jobs    map[string]*nowJob
	seq     uint64
	scans   map[string]*scanEntry // by pair key; "" for an unkeyed scan
	scanSeq uint64
	lastOK  time.Time
	lastBad bool // the most recent pass finished with something failing
}

func (e *Engine) nowSt() *nowState {
	e.nowOnce.Do(func() { e.now = &nowState{jobs: map[string]*nowJob{}} })
	return e.now
}

// jobStart records a small transfer as running and returns the function that
// ends it. Safe to call done more than once.
func (e *Engine) jobStart(abs, kind string, size int64) (done func(err error)) {
	dir, rel := e.splitByPair(abs)
	return e.jobStartIn(dir, rel, abs, kind, size)
}

// splitByPair finds the configured pair whose folder contains abs and returns
// that folder and the pair-relative, slash-separated path. With no containing
// pair (an on-demand mount has none) it returns "" and abs itself, so a reader
// can always rebuild the absolute path: LocalDir joined with Path, or Path.
func (e *Engine) splitByPair(abs string) (localDir, rel string) {
	var dirs []string
	if e.nowPairDirs != nil {
		dirs = e.nowPairDirs()
	} else if pairs, err := e.Pairs(); err == nil {
		for _, p := range pairs {
			dirs = append(dirs, p.LocalDir)
		}
	}
	best := ""
	for _, d := range dirs {
		if d == "" || len(d) <= len(best) {
			continue
		}
		r, err := filepath.Rel(d, abs)
		if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			continue
		}
		best, rel = d, filepath.ToSlash(r)
	}
	if best == "" {
		return "", abs
	}
	return best, rel
}

// jobStartIn is jobStart for a transfer whose sync folder and relative path are known.
func (e *Engine) jobStartIn(localDir, rel, abs, kind string, size int64) (done func(err error)) {
	s := e.nowSt()
	s.mu.Lock()
	s.seq++
	j := &nowJob{seq: s.seq, NowJob: NowJob{Kind: kind, LocalDir: localDir, Path: rel, Size: size, State: "running"}}
	s.jobs[abs] = j
	s.mu.Unlock()
	return func(error) {
		s.mu.Lock()
		// Only remove our own record: a retry may have registered the same path.
		if s.jobs[abs] == j {
			delete(s.jobs, abs)
		}
		s.mu.Unlock()
	}
}

// jobProgress adds delta sent bytes to the running transfer at abs, if any.
func (e *Engine) jobProgress(abs string, delta int64) {
	s := e.nowSt()
	s.mu.Lock()
	if j := s.jobs[abs]; j != nil {
		j.Sent += delta
	}
	s.mu.Unlock()
}

// actionKind maps a transfer action to a NowJob kind ("" for anything else).
func actionKind(k engine.ActionKind) string {
	switch k {
	case engine.ActUpload:
		return "upload"
	case engine.ActDownload:
		return "download"
	}
	return ""
}

// The executor hooks identify a transfer by its action, not a returned closure,
// so these three wrap the same bookkeeping by absolute path. Actions that are
// not uploads or downloads (mkdir, delete, move) are ignored.

func (e *Engine) jobActionBegin(p Pair, a engine.Action, remote map[string]engine.RemoteState) {
	kind := actionKind(a.Kind)
	if kind == "" {
		return
	}
	abs := filepath.Join(p.LocalDir, filepath.FromSlash(a.Path))
	e.jobStartIn(p.LocalDir, a.Path, abs, kind, transferSize(p, a, remote))
}

func (e *Engine) jobActionProgress(p Pair, a engine.Action, delta int64) {
	if actionKind(a.Kind) == "" {
		return
	}
	e.jobProgress(filepath.Join(p.LocalDir, filepath.FromSlash(a.Path)), delta)
}

func (e *Engine) jobActionEnd(p Pair, a engine.Action) {
	if actionKind(a.Kind) == "" {
		return
	}
	abs := filepath.Join(p.LocalDir, filepath.FromSlash(a.Path))
	s := e.nowSt()
	s.mu.Lock()
	delete(s.jobs, abs)
	s.mu.Unlock()
}

// NowJobs returns the transfers in flight: running small ones, then every
// lane entry (Large), running first.
func (e *Engine) NowJobs() []NowJob {
	s := e.nowSt()
	s.mu.Lock()
	small := make([]*nowJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		small = append(small, j)
	}
	sort.Slice(small, func(a, b int) bool { return small[a].seq < small[b].seq })
	out := make([]NowJob, 0, len(small))
	for _, j := range small {
		out = append(out, j.NowJob)
	}
	s.mu.Unlock()

	var running, rest []NowJob
	for _, l := range e.LaneEntries() {
		kind := "download"
		if l.Upload {
			kind = "upload"
		}
		nj := NowJob{Kind: kind, LocalDir: l.LocalDir, Path: l.Path, Size: l.Size, Sent: l.Sent, Large: true, State: l.State}
		if l.State == "running" {
			running = append(running, nj)
		} else {
			rest = append(rest, nj)
		}
	}
	out = append(out, running...)
	return append(out, rest...)
}

// scanTickCount records the scan's stage and count for ScanNow. Unlike scanTick
// it is neither throttled nor gated by the quiet period: it is a cheap
// snapshot, read on demand.
func (e *Engine) scanTickCount(phase string, count int, folder string) {
	e.scanTickCountFor("", phase, count, folder)
}

// scanEnd marks the scan finished (the plan is computed, or failed).
func (e *Engine) scanEnd() { e.scanEndFor("") }

// scanEntry is one pair's scan; pairs scan concurrently, so each has its own.
type scanEntry struct {
	ScanNow
	seq uint64 // last update, to pick the freshest phase/folder
}

// scanTickCountFor is scanTickCount for the scan of pair pk.
func (e *Engine) scanTickCountFor(pk, phase string, count int, folder string) {
	s := e.nowSt()
	s.mu.Lock()
	if s.scans == nil {
		s.scans = map[string]*scanEntry{}
	}
	s.scanSeq++
	s.scans[pk] = &scanEntry{ScanNow: ScanNow{Active: true, Phase: phase, Count: count, Folder: folder}, seq: s.scanSeq}
	s.mu.Unlock()
}

// scanEndFor ends pair pk's scan only; other pairs may still be scanning.
func (e *Engine) scanEndFor(pk string) {
	s := e.nowSt()
	s.mu.Lock()
	delete(s.scans, pk)
	s.mu.Unlock()
}

// ScanNow returns the scan's current stage: Active while any pair scans, with
// the counts summed and the most recently updated phase and folder.
func (e *Engine) ScanNow() ScanNow {
	s := e.nowSt()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out ScanNow
	var newest uint64
	for _, en := range s.scans {
		out.Active = true
		out.Count += en.Count
		if en.seq >= newest {
			newest, out.Phase, out.Folder = en.seq, en.Phase, en.Folder
		}
	}
	return out
}

// noteFullCheck records the end of a full pass: ok = nothing failed.
func (e *Engine) noteFullCheck(at time.Time, ok bool) {
	s := e.nowSt()
	s.mu.Lock()
	if ok {
		s.lastOK = at
	}
	s.lastBad = !ok
	s.mu.Unlock()
}

// LastFullCheck returns the end of the last full pass that finished with no
// folder failing, and whether the most recent pass finished with one failing.
func (e *Engine) LastFullCheck() (at time.Time, incomplete bool) {
	s := e.nowSt()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOK, s.lastBad
}

// otherPairsHealthy is true when no folder other than p is failing. SyncOnce
// asks it before notePairResult has recorded p's own outcome.
func (e *Engine) otherPairsHealthy(p Pair) bool {
	for dir := range e.PairHealths() {
		if dir != p.LocalDir {
			return false
		}
	}
	return true
}
