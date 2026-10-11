package agent

import (
	"container/heap"
	"context"
	"errors"
	"math"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transfer"
)

// Download these first (Deck #766). A first sync of a big account takes hours,
// and the folder the user needs today could come last: the clone used to list
// top-level folders in server order and download them in whatever order their
// listings happened to finish. The user can now choose folders (any depth) to
// come first, in the setup wizard or while the clone runs, and the clone
// schedules its work around them:
//
//   - Listings: one recursive PROPFIND per top-level folder, as before, but
//     queued; a listing inside a chosen folder starts before the others.
//   - A chosen folder deeper down is listed ON ITS OWN ("carved" out of its
//     top-level folder's listing) while that listing has not been planned yet,
//     so it does not wait for a listing of everything around it. The bigger
//     listing then leaves out what the carve covers. Once a listing has been
//     planned, nothing more is carved out of it: its files are already queued,
//     and a new choice just reorders them.
//   - Downloads: one shared queue of directories, each holding its files in
//     path order, drained file by file by a fixed pool. A directory inside a
//     chosen folder ranks by the folder's place in the list; everything else
//     keeps the order it was queued in. Changing the choice re-ranks what is
//     still waiting; files already downloading finish first.
//
// This is the clone's own scheduling, separate from the large-file lane
// (lane.go), which only serves the normal sync.

// noRank is the rank of work in no chosen folder: after all of it.
const noRank = math.MaxInt

// errCloneFinished refuses a choice for a pair whose first sync is already done.
var errCloneFinished = errors.New("the first sync of this folder has already finished")

// maxCloneFirst bounds the list; a choice is a handful of folders.
const maxCloneFirst = 50

// normCloneFirst cleans a list of chosen folders: slash-separated and trimmed,
// without blanks (the pair root is not a choice), "." or ".." parts, and
// repeats, in the order given.
func normCloneFirst(paths []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, p := range paths {
		p = strings.Trim(strings.TrimSpace(strings.ReplaceAll(p, "\\", "/")), "/")
		if p == "" || seen[p] || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) == maxCloneFirst {
			break
		}
	}
	return out
}

// atOrUnder reports whether rel is dir itself or inside it.
func atOrUnder(rel, dir string) bool {
	return dir == "" || rel == dir || strings.HasPrefix(rel, dir+"/")
}

// cloneRank is the place in first of the first chosen folder holding rel.
func cloneRank(first []string, rel string) int {
	for i, p := range first {
		if atOrUnder(rel, p) {
			return i
		}
	}
	return noRank
}

// cloneListing is one recursive listing the clone still has to make, or has
// made.
type cloneListing struct {
	rel    string // pair-relative folder listed (with everything under it)
	full   string // files-root-relative, as PROPFIND takes it
	carved bool   // a chosen folder listed on its own, out of a bigger listing
	sealed bool   // being (or been) planned: nothing more is carved out of it
	rank   int
	seq    int64
}

// cloneDir is one directory's downloads from one listing.
type cloneDir struct {
	dir     string          // pair-relative directory holding the files
	actions []engine.Action // its downloads, in path order
	next    int             // the first not yet handed out
	remote  map[string]engine.RemoteState
	rank    int
	seq     int64
	index   int // in the heap
}

// cloneDirHeap orders directories by rank, then by the order they were queued.
type cloneDirHeap []*cloneDir

func (h cloneDirHeap) Len() int { return len(h) }
func (h cloneDirHeap) Less(i, j int) bool {
	if h[i].rank != h[j].rank {
		return h[i].rank < h[j].rank
	}
	return h[i].seq < h[j].seq
}
func (h cloneDirHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *cloneDirHeap) Push(x any) {
	d := x.(*cloneDir)
	d.index = len(*h)
	*h = append(*h, d)
}
func (h *cloneDirHeap) Pop() any {
	old := *h
	d := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return d
}

// cloneSched is a running clone's work queue: listings to make and files to
// download, both ordered by the folders chosen to come first. Safe for
// concurrent use; the wait methods block until there is work or none can come.
type cloneSched struct {
	mu   sync.Mutex
	cond *sync.Cond

	root    string            // the pair's remote root, files-root-relative
	ignored func(string) bool // pair-relative paths the clone skips (nil = none)
	first   []string

	pending  []*cloneListing // listings not started yet
	listings []*cloneListing // every listing queued so far, for carving
	running  int             // listings in progress
	dirs     cloneDirHeap
	seq      int64
	stopped  bool
}

func newCloneSched(root string, ignored func(string) bool) *cloneSched {
	s := &cloneSched{root: strings.Trim(root, "/"), ignored: ignored}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// fullPath maps a pair-relative folder to the path PROPFIND takes.
func (s *cloneSched) fullPath(rel string) string {
	if s.root == "" {
		return rel
	}
	return s.root + "/" + rel
}

// addListing queues a top-level folder's listing.
func (s *cloneSched) addListing(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queueListingLocked(rel, false)
}

func (s *cloneSched) queueListingLocked(rel string, carved bool) {
	s.seq++
	l := &cloneListing{rel: rel, full: s.fullPath(rel), carved: carved, rank: cloneRank(s.first, rel), seq: s.seq}
	s.pending = append(s.pending, l)
	s.listings = append(s.listings, l)
	s.cond.Broadcast()
}

// setFirst replaces the chosen folders: re-ranks waiting work and carves a
// chosen folder out of a listing that has not been planned yet.
func (s *cloneSched) setFirst(first []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.first = append([]string(nil), first...)
	for _, p := range s.first {
		s.carveLocked(p)
	}
	for _, l := range s.pending {
		l.rank = cloneRank(s.first, l.rel)
	}
	for _, d := range s.dirs {
		d.rank = cloneRank(s.first, d.dir)
	}
	heap.Init(&s.dirs)
	s.cond.Broadcast()
}

// carveLocked lists a chosen folder on its own when the deepest listing that
// holds it has not been planned yet. Nothing to do when that listing IS the
// folder, when it has been planned (its files are queued already), or when
// no listing holds it (the folder is in the pair root's own listing, or not
// on the server at all).
func (s *cloneSched) carveLocked(p string) {
	if s.ignored != nil && s.ignored(p) {
		return
	}
	var deepest *cloneListing
	for _, l := range s.listings {
		if atOrUnder(p, l.rel) && (deepest == nil || len(l.rel) > len(deepest.rel)) {
			deepest = l
		}
	}
	if deepest == nil || deepest.rel == p || deepest.sealed {
		return
	}
	s.queueListingLocked(p, true)
}

// nextListing hands out the best waiting listing, or nil once there are none
// and none in progress (a listing in progress can still have a chosen folder
// carved out of it, so an idle worker waits for that rather than leaving).
func (s *cloneSched) nextListing() *cloneListing {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.stopped {
			return nil
		}
		if len(s.pending) > 0 {
			best := 0
			for i, l := range s.pending {
				b := s.pending[best]
				if l.rank < b.rank || (l.rank == b.rank && l.seq < b.seq) {
					best = i
				}
			}
			l := s.pending[best]
			s.pending = append(s.pending[:best], s.pending[best+1:]...)
			s.running++
			return l
		}
		if s.running == 0 {
			return nil
		}
		s.cond.Wait()
	}
}

// seal marks a listing as being planned and returns the folders carved out of
// it, which its plan must leave to their own listings. From here on nothing
// more is carved out of it.
func (s *cloneSched) seal(l *cloneListing) (carved []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l.sealed = true
	for _, o := range s.listings {
		if o.carved && o != l && o.rel != l.rel && atOrUnder(o.rel, l.rel) {
			carved = append(carved, o.rel)
		}
	}
	return carved
}

// listingDone ends a listing handed out by nextListing (after its downloads
// are queued, so a download worker never sees "nothing left" in between).
func (s *cloneSched) listingDone() {
	s.mu.Lock()
	s.running--
	s.cond.Broadcast()
	s.mu.Unlock()
}

// queueDownloads groups a listing's downloads by directory, each in path
// order, and queues the directories in path order.
func (s *cloneSched) queueDownloads(actions []engine.Action, remote map[string]engine.RemoteState) {
	if len(actions) == 0 {
		return
	}
	byDir := make(map[string][]engine.Action)
	for _, a := range actions {
		d := dirParent(a.Path)
		byDir[d] = append(byDir[d], a)
	}
	names := make([]string, 0, len(byDir))
	for d := range byDir {
		names = append(names, d)
	}
	sort.Strings(names)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range names {
		acts := byDir[d]
		sort.Slice(acts, func(i, j int) bool { return acts[i].Path < acts[j].Path })
		s.seq++
		heap.Push(&s.dirs, &cloneDir{dir: d, actions: acts, remote: remote, rank: cloneRank(s.first, d), seq: s.seq})
	}
	s.cond.Broadcast()
}

// nextDownload hands out the best waiting file with its listing, or ok=false
// once nothing is queued and no listing can queue more.
func (s *cloneSched) nextDownload() (a engine.Action, remote map[string]engine.RemoteState, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.stopped {
			return engine.Action{}, nil, false
		}
		if len(s.dirs) > 0 {
			d := s.dirs[0]
			a, remote = d.actions[d.next], d.remote
			d.next++
			if d.next == len(d.actions) {
				heap.Pop(&s.dirs)
				d.actions, d.remote = nil, nil // let a finished listing go
			}
			return a, remote, true
		}
		if len(s.pending) == 0 && s.running == 0 {
			return engine.Action{}, nil, false
		}
		s.cond.Wait()
	}
}

// stop wakes every waiting worker and makes them all leave (the clone was
// cancelled).
func (s *cloneSched) stop() {
	s.mu.Lock()
	s.stopped = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// chosen returns the current choice.
func (s *cloneSched) chosen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.first...)
}

// cloneMkdirs creates a listing's folders (parents first, Executor.Run sorts
// them) before any of its files are queued, so a file never lands ahead of
// the folder baseline it belongs under.
func cloneMkdirs(ctx context.Context, x *transfer.Executor, actions []engine.Action, note func(transfer.Stats, error)) {
	var dirs []engine.Action
	for _, a := range actions {
		if a.Kind == engine.ActCreateLocalDir {
			dirs = append(dirs, a)
		}
	}
	if len(dirs) > 0 {
		s, err := x.Run(ctx, dirs)
		note(s, err)
	}
}

// downloadsOf picks a plan's downloads.
func downloadsOf(actions []engine.Action) []engine.Action {
	var out []engine.Action
	for _, a := range actions {
		if a.Kind == engine.ActDownload {
			out = append(out, a)
		}
	}
	return out
}

// CloneRun is a first sync (initial clone) in progress, for the UI.
type CloneRun struct {
	LocalDir   string   `json:"localDir"`
	RemoteRoot string   `json:"remoteRoot"`
	First      []string `json:"first"` // folders downloading first, pair-relative, in order
}

type cloneRunning struct {
	localDir, remoteRoot string
	sched                *cloneSched
}

func (e *Engine) trackClone(pk string, p Pair, s *cloneSched) {
	e.cloneMu.Lock()
	if e.clones == nil {
		e.clones = make(map[string]*cloneRunning)
	}
	e.clones[pk] = &cloneRunning{localDir: p.LocalDir, remoteRoot: strings.Trim(p.RemoteRoot, "/"), sched: s}
	e.cloneMu.Unlock()
}

func (e *Engine) untrackClone(pk string) {
	e.cloneMu.Lock()
	delete(e.clones, pk)
	e.cloneMu.Unlock()
}

// CloneRuns lists the first syncs in progress, in local-folder order.
func (e *Engine) CloneRuns() []CloneRun {
	e.cloneMu.Lock()
	runs := make([]*cloneRunning, 0, len(e.clones))
	for _, r := range e.clones {
		runs = append(runs, r)
	}
	e.cloneMu.Unlock()
	if len(runs) == 0 {
		return nil
	}
	out := make([]CloneRun, 0, len(runs))
	for _, r := range runs {
		out = append(out, CloneRun{LocalDir: r.localDir, RemoteRoot: r.remoteRoot, First: r.sched.chosen()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LocalDir < out[j].LocalDir })
	return out
}

// SetCloneFirst sets the folders (pair-relative, any depth, in order) that a
// pair's first sync downloads before everything else. It is saved with the
// pair, so it holds across a restart that interrupts the clone, and reorders
// a clone already running. An empty list goes back to the normal order. It
// can be set before the pair's first sync starts; once that sync has
// finished there is nothing left to order, and it is refused.
func (e *Engine) SetCloneFirst(localDir, remoteRoot string, paths []string) error {
	remoteRoot = strings.Trim(remoteRoot, "/")
	pk := PairKey(localDir, remoteRoot)
	st, err := e.getStore()
	if err != nil {
		return err
	}
	if status, _ := st.CloneStatus(pk); status == "done" {
		return errCloneFinished
	}
	paths = normCloneFirst(paths)
	if err := st.SetCloneFirst(pk, paths); err != nil {
		return err
	}
	e.cloneMu.Lock()
	r := e.clones[pk]
	e.cloneMu.Unlock()
	if r != nil {
		r.sched.setFirst(paths)
		e.emitProgress()
	}
	return nil
}
