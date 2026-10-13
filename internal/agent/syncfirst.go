package agent

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
)

// "Sync this first" (Deck #702, stage 3): Explorer's context menu asks for one
// file or folder of a live sync folder to go ahead of the rest.
//
//   - A large transfer waiting in the lane moves to the front (never past a
//     running one: see lane.first). A file set aside comes back.
//   - Anything else is handed to its folder's watcher as if it had just
//     changed, which syncs exactly those paths in a quick pass (SyncPaths).
//     That works while a failed pass is being backed off from: the backoff
//     only spaces out the server-walking polls, never a sync of named paths.
//   - A folder does both for everything beneath it, and asks for the server's
//     side of the folder as "Sync now" does.
//
// Nothing here plans or runs a transfer itself, so every guard of a normal
// pass still judges what follows. A pass that is already running is not
// interrupted: the quick pass runs when it ends.

// SyncFirstKind says how a "Sync this first" request ended.
type SyncFirstKind int

const (
	// SyncFirstOutside: the path is in none of this account's sync folders.
	SyncFirstOutside SyncFirstKind = iota
	// SyncFirstPaused: syncing is paused. Nothing was done.
	SyncFirstPaused
	// SyncFirstNotSynced: the path is left out of syncing (an ignore rule, a
	// folder the user unticked, a file they chose never to sync).
	SyncFirstNotSynced
	// SyncFirstHeld: its sync folder is not syncing at the moment. Why says
	// why, as a sentence for the user.
	SyncFirstHeld
	// SyncFirstGone: the path is no longer there.
	SyncFirstGone
	// SyncFirstAsked: done. The other fields say what was asked for.
	SyncFirstAsked
)

// SyncFirstResult is what SyncFirst did with a path.
type SyncFirstResult struct {
	Kind SyncFirstKind
	Why  string // for SyncFirstHeld
	// Lane is how many large transfers moved to the front of the queue or
	// came back from being set aside.
	Lane int
	// Running: the path's large transfer was already running.
	Running bool
	// Paths is how many paths were handed to an immediate sync.
	Paths int
	// Whole: the whole sync folder was asked to sync instead, because the
	// folder holds too many changes to name one by one.
	Whole bool
}

// syncFirstBatch is how many entries of a folder are checked against the
// baseline at a time.
const syncFirstBatch = 256

// SyncFirst asks for the file or folder at the local path abs to sync ahead
// of the rest. It returns at once for a file; for a folder it first walks the
// folder for changes that are still to be sent, so call it off the UI thread.
func (e *Engine) SyncFirst(abs string) SyncFirstResult {
	abs = filepath.Clean(abs)
	p, rel, ok := e.pairHolding(abs)
	if !ok {
		return SyncFirstResult{Kind: SyncFirstOutside}
	}
	if e.Paused() {
		return SyncFirstResult{Kind: SyncFirstPaused}
	}
	if why := e.HeldReason(p.LocalDir); why != "" {
		return SyncFirstResult{Kind: SyncFirstHeld, Why: why}
	}
	if f, has := e.guardFor(p); has && f.Frozen != nil {
		return SyncFirstResult{Kind: SyncFirstHeld, Why: "Syncing this folder is paused for review: " + f.Frozen.Reason +
			". Review it in Settings and resume if this was expected."}
	}
	ig := e.ignoreFor(p)
	if rel != "" && (ig.Match(rel) || e.blacklisted(abs)) {
		return SyncFirstResult{Kind: SyncFirstNotSynced}
	}
	fi, err := os.Stat(abs)
	if err != nil {
		// Gone since the menu was opened. A sync of the path now would be a
		// delete on the server; that is the watcher's to send, not the menu's.
		return SyncFirstResult{Kind: SyncFirstGone}
	}

	pk := PairKey(p.LocalDir, p.RemoteRoot)
	l := e.transferLane()
	res := SyncFirstResult{Kind: SyncFirstAsked}
	// Whatever a pass hands the lane for this path from now on joins the
	// front: a large file a pass has yet to plan, or one coming back below.
	l.prefer(abs, lanePreferFor)

	if !fi.IsDir() {
		switch l.stateAt(abs) {
		case laneRunning:
			res.Running = true
		case laneWaiting:
			if l.first(abs) {
				res.Lane = 1
			}
		case laneParked:
			// Asking for it by name outranks having set it aside. It leaves
			// the lane, a quick pass plans it afresh (laneDone), and prefer
			// puts it at the front when that pass hands it back.
			if l.unpark(abs) {
				res.Lane = 1
			}
		default:
			res.Paths = e.nudgeAll(pk, []string{abs})
		}
		slog.Info("sync this first", "path", rel, "lane", res.Lane, "running", res.Running, "paths", res.Paths)
		return res
	}

	// A folder: its waiting large transfers first, in path order. The ones
	// set aside stay set aside: each was set aside by name.
	res.Lane = l.firstUnder(abs)
	pending, tooMany, err := e.localPending(p, pk, rel, ig, l)
	switch {
	case err != nil:
		slog.Warn("sync this first: could not look through the folder; syncing the whole sync folder instead", "path", rel, "err", err)
		res.Whole = e.triggerPair(pk, true)
	case tooMany:
		res.Whole = e.triggerPair(pk, true)
	default:
		res.Paths = e.nudgeAll(pk, pending)
		// What changed on the server is not in that list. Ask for it as
		// "Sync now" does, which also lifts a failure backoff.
		e.triggerPair(pk, false)
	}
	slog.Info("sync this first", "folder", rel, "lane", res.Lane, "paths", res.Paths, "whole", res.Whole)
	return res
}

// pairHolding returns the sync pair whose local folder holds abs, and abs
// relative to it ("" for the folder itself). With one folder inside another,
// the innermost wins.
func (e *Engine) pairHolding(abs string) (Pair, string, bool) {
	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return Pair{}, "", false
	}
	var best *config.SyncPair
	for i := range pairs {
		if !within(abs, pairs[i].LocalDir) {
			continue
		}
		if best == nil || len(filepath.Clean(pairs[i].LocalDir)) > len(filepath.Clean(best.LocalDir)) {
			best = &pairs[i]
		}
	}
	if best == nil {
		return Pair{}, "", false
	}
	rel, err := filepath.Rel(filepath.Clean(best.LocalDir), abs)
	if err != nil {
		return Pair{}, "", false
	}
	if rel = filepath.ToSlash(rel); rel == "." {
		rel = ""
	}
	return Pair{LocalDir: best.LocalDir, RemoteRoot: best.RemoteRoot, Excludes: best.Excludes}, rel, true
}

// blacklisted reports whether the user chose never to sync the file at abs.
func (e *Engine) blacklisted(abs string) bool {
	set, err := e.dirs.LoadBlacklist()
	return err == nil && set[config.PathKey(abs)]
}

// localPending walks the folder rel of pair p for what is still to be sent:
// files and folders the baseline has no row for, and files whose size or
// time differ from their row. It returns their local paths. Ignored paths
// and files the lane holds are left out. tooMany is set, and the walk ends
// early, once there are more than one quick pass takes (maxSyncPaths).
//
// A file that is gone locally is NOT in the list, on purpose: sending a
// delete is left to the passes that see the whole folder.
func (e *Engine) localPending(p Pair, pk, rel string, ig *engine.Ignore, l *lane) (pending []string, tooMany bool, err error) {
	st, err := e.getStore()
	if err != nil {
		return nil, false, err
	}
	type entry struct {
		rel, abs string
		isDir    bool
		size     int64
		mtime    time.Time
	}
	var batch []entry
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		rels := make([]string, len(batch))
		for i, en := range batch {
			rels[i] = en.rel
		}
		// A fresh subset each time: the resident baseline map itself belongs
		// to the passes and must not be read from outside one.
		base, lerr := st.LoadBaselinePaths(pk, rels)
		if lerr != nil {
			return lerr
		}
		for _, en := range batch {
			b, tracked := base[en.rel]
			changed := !tracked || b.IsDir != en.isDir ||
				(!en.isDir && engine.LocalFileChanged(engine.LocalState{Size: en.size, MTime: en.mtime}, b))
			if changed && !l.has(pk, en.rel) {
				pending = append(pending, en.abs)
			}
		}
		batch = batch[:0]
		return nil
	}

	root := filepath.Join(p.LocalDir, filepath.FromSlash(rel))
	werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, derr error) error {
		if derr != nil {
			// Unreadable: leave it to a full pass, which reports it.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		r, rerr := filepath.Rel(p.LocalDir, path)
		if rerr != nil {
			return nil
		}
		if r = filepath.ToSlash(r); r == "." {
			return nil // the sync folder itself has no row and nothing to send
		}
		if ig.Match(r) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		en := entry{rel: r, abs: path, isDir: d.IsDir()}
		if !en.isDir {
			fi, ierr := d.Info()
			if ierr != nil {
				return nil // vanished mid-walk
			}
			en.size, en.mtime = fi.Size(), fi.ModTime()
		}
		batch = append(batch, en)
		if len(batch) >= syncFirstBatch {
			if ferr := flush(); ferr != nil {
				return ferr
			}
			if len(pending) > maxSyncPaths {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if werr != nil {
		return nil, false, werr
	}
	if err := flush(); err != nil {
		return nil, false, err
	}
	if len(pending) > maxSyncPaths {
		return nil, true, nil
	}
	return pending, false, nil
}

// nudgeAll hands the local paths abs to the pair's watcher to sync as if it
// had seen them change, and returns how many it handed over (0 with no
// watcher: the folder is not being synced, or the engine has yet to start).
// Unlike nudgePath it never drops one: a watcher busy with a pass takes them
// when the pass ends, so this waits for it on a goroutine of its own, which
// ends with the watcher.
func (e *Engine) nudgeAll(pk string, abs []string) int {
	if len(abs) == 0 {
		return 0
	}
	e.watchMu.Lock()
	ch, done := e.nudges[pk], e.watchDone[pk]
	e.watchMu.Unlock()
	if ch == nil || done == nil {
		return 0
	}
	go func() {
		for _, a := range abs {
			select {
			case ch <- a:
			case <-done:
				return
			}
		}
	}()
	return len(abs)
}

// triggerPair asks one pair's watcher for a sync: a full pass that walks the
// local folder (full), or the pass "Sync now" asks for, which fetches what
// changed on the server and is not held back by a failure backoff. False when
// the pair has no watcher. A trigger already waiting is enough: the send
// never blocks.
func (e *Engine) triggerPair(pk string, full bool) bool {
	e.watchMu.Lock()
	ch := e.triggersUser[pk]
	if full {
		ch = e.triggersFull[pk]
	}
	e.watchMu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- struct{}{}:
	default:
	}
	return true
}
