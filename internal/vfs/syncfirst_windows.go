//go:build windows

package vfs

import (
	"os"
	"path/filepath"
	"strings"
)

// "Sync this first" in an on-demand folder (Deck #702, stage 3).
//
// An on-demand folder has no queue of passes to jump: each upload runs on its
// own as soon as its file settles. What can keep a file waiting is a retry
// timer (a failed upload backs off for up to half an hour, a file another
// program holds is looked at once a minute) and the download queue of files
// the user chose to keep on this device, which takes two at a time. SyncFirst
// cuts both short for one file, or for everything beneath a folder.
//
// It never downloads a file the user has not already asked to keep on this
// device. An online-only file is in step with the server; fetching it is what
// "Always keep on this device" and opening it are for, and on a folder it
// would download the whole tree.

// SyncFirst asks for the file or folder at path to sync ahead of the rest.
// Safe to call from any goroutine; it starts work and returns.
func (w *Watcher) SyncFirst(path string) SyncFirstResult {
	if w.ctx.Err() != nil {
		return SyncFirstResult{}
	}
	if w.paused() {
		return SyncFirstResult{Paused: true}
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(w.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return SyncFirstResult{NotSynced: true}
	}
	if rel != "." && skipName(rel) {
		return SyncFirstResult{NotSynced: true}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return SyncFirstResult{} // gone since the menu was opened
	}
	if info.IsDir() {
		return w.syncFirstDir(path)
	}

	var res SyncFirstResult
	defer func() {
		w.ops.Log("vfs sync this first %s: %d upload(s), %d download(s)", w.remoteFor(path), res.Uploads, res.Downloads)
	}()
	// An upload waiting on a timer (a retry backing off, a file another
	// program held) starts now; one already running is left to it.
	switch w.uploadNow(path, false) {
	case uploadBusy:
		res.Busy = true
		return res
	case uploadStarted:
		res.Uploads = 1
		return res
	}
	if w.downloading(path) {
		res.Busy = true
		return res
	}
	ch, err := cfInspect(path)
	if err != nil {
		w.ops.Log("vfs sync this first %s: %v", path, err)
		return res
	}
	online := cfIsDehydrated(info, filepath.ToSlash(path))
	switch {
	case ch.NeedsUpload && !online:
		// Changed, and nothing is on its way to send it (an event that was
		// lost): send it now.
		if w.uploadNow(path, true) == uploadStarted {
			res.Uploads = 1
		}
	case cfPinnedDehydrated(path):
		res.Downloads, res.Busy = w.hydrateFirstNow(path)
	case online:
		res.OnlineOnly = true
	}
	return res
}

// syncFirstDir is SyncFirst for a folder: every upload waiting on a timer
// beneath it starts now, and the waiting downloads of its kept-on-device
// files move ahead of the others. Nothing else is downloaded.
func (w *Watcher) syncFirstDir(dir string) SyncFirstResult {
	var res SyncFirstResult
	under := func(p string) bool {
		pl, dl := strings.ToLower(p), strings.ToLower(dir)
		return pl == dl || strings.HasPrefix(pl, strings.TrimRight(dl, `\/`)+string(filepath.Separator))
	}
	w.mu.Lock()
	for p, t := range w.upload {
		if !under(p) {
			continue
		}
		if _, busy := w.inflight[strings.ToLower(p)]; busy {
			continue // going up now; its timer is the pass after this one
		}
		t.Reset(0)
		res.Uploads++
	}
	for k := range w.inflight {
		if under(k) {
			res.Busy = true
		}
	}
	var first, rest []string
	for _, p := range w.hydratePending {
		if under(p) {
			first = append(first, p)
		} else {
			rest = append(rest, p)
		}
	}
	if len(first) > 0 {
		w.hydratePending = rest
		w.hydrateFirst = append(w.hydrateFirst, first...)
		res.Downloads = len(first)
	}
	w.mu.Unlock()
	if res.Downloads > 0 {
		w.wakeFirstHydrator()
		w.wakeHydrator()
	}
	// The server's side of the folder, and any changed file the timers have
	// lost track of: a reconcile pass, soon.
	if w.ops.List != nil {
		w.Poke()
	}
	w.ops.Log("vfs sync this first %s: %d upload(s), %d download(s)", w.remoteFor(dir), res.Uploads, res.Downloads)
	return res
}

const (
	uploadNone    = iota // nothing waiting, and none was asked for
	uploadStarted        // an upload was started now
	uploadBusy           // one is running already
)

// uploadNow starts the upload path is waiting for, if any: a retry or
// debounce timer is cut short. With arm set it starts one even when nothing
// was waiting. Paths compare without case: the menu's spelling of a path
// need not be the one the change events used.
func (w *Watcher) uploadNow(path string, arm bool) int {
	key := strings.ToLower(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, busy := w.inflight[key]; busy {
		return uploadBusy
	}
	for p, t := range w.upload {
		if strings.ToLower(p) == key {
			t.Reset(0)
			return uploadStarted
		}
	}
	if !arm {
		return uploadNone
	}
	w.upload[path] = w.after(0, func() {
		w.mu.Lock()
		delete(w.upload, path)
		w.mu.Unlock()
		w.handleChange(path)
	})
	return uploadStarted
}

// downloading reports whether a download of path is running right now (not
// merely waiting its turn).
func (w *Watcher) downloading(path string) bool {
	key := strings.ToLower(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.hydrating[key] {
		return false
	}
	for _, q := range [][]string{w.hydratePending, w.hydrateFirst} {
		for _, p := range q {
			if strings.ToLower(p) == key {
				return false
			}
		}
	}
	return true
}

// hydrateFirstNow puts the download of a kept-on-device file ahead of the
// others. moved is 1 when it did; busy when the file is downloading already.
// A failure it was backing off from is forgotten: the user asked by name.
func (w *Watcher) hydrateFirstNow(path string) (moved int, busy bool) {
	key := strings.ToLower(path)
	w.mu.Lock()
	if w.hydrating[key] {
		at := -1
		for i, p := range w.hydratePending {
			if strings.ToLower(p) == key {
				at = i
				break
			}
		}
		if at < 0 {
			asked := false
			for _, p := range w.hydrateFirst {
				if strings.ToLower(p) == key {
					asked = true
					break
				}
			}
			w.mu.Unlock()
			return 0, !asked // asked for already, or downloading now
		}
		path = w.hydratePending[at]
		w.hydratePending = append(w.hydratePending[:at], w.hydratePending[at+1:]...)
	} else {
		w.hydrating[key] = true
	}
	delete(w.hydrateNext, key)
	w.hydrateFirst = append(w.hydrateFirst, path)
	w.mu.Unlock()
	w.wakeFirstHydrator()
	w.wakeHydrator()
	return 1, false
}

// wakeFirstHydrator nudges hydrateFirstLoop, as wakeHydrator nudges the others.
func (w *Watcher) wakeFirstHydrator() {
	select {
	case w.hydrateFirstWake <- struct{}{}:
	default:
	}
}

// nextFirstHydration takes the oldest download the user asked to go first.
func (w *Watcher) nextFirstHydration() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.hydrateFirst) == 0 {
		return "", false
	}
	path := w.hydrateFirst[0]
	w.hydrateFirst = w.hydrateFirst[1:]
	return path, true
}

// hydrateFirstLoop is the one extra download worker for files the user asked
// to go first: it takes only from hydrateFirst, so it is free whenever such a
// file arrives, whatever the other workers are on. Started by New; it exits
// when the watcher is closed.
func (w *Watcher) hydrateFirstLoop() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.hydrateFirstWake:
		}
		for {
			if w.ctx.Err() != nil {
				return
			}
			if w.paused() {
				break // releaseHeld wakes us
			}
			path, ok := w.nextFirstHydration()
			if !ok {
				break
			}
			w.hydratePinned(path)
		}
	}
}
