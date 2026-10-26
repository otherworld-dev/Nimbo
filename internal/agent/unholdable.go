package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transport"
	"github.com/otherworld/nimbo/internal/winname"
)

// UnholdableReason is what "Can't sync" says about a server item whose name
// Windows cannot hold (Deck #984).
const UnholdableReason = "Its name ends in a space or a full stop, which Windows can't use. Rename it on the server to sync it."

// remoteParent is the files-root-relative parent of a server path ("" at the
// top).
func remoteParent(remote string) string {
	remote = strings.Trim(remote, "/")
	if i := strings.LastIndex(remote, "/"); i >= 0 {
		return remote[:i]
	}
	return ""
}

// loadUnholdLocked reads the saved list once. Caller holds unholdMu.
func (e *Engine) loadUnholdLocked() {
	if e.unholdLoaded {
		return
	}
	e.unholdLoaded = true
	e.unhold = map[string]config.Unholdable{}
	list, err := e.dirs.LoadUnholdable()
	if err != nil {
		slog.Warn("cannot read the unsyncable-name list", "err", err)
		return
	}
	for _, u := range list {
		e.unhold[strings.Trim(u.Remote, "/")] = u
	}
}

// saveUnholdLocked writes the list. Caller holds unholdMu.
func (e *Engine) saveUnholdLocked() {
	list := make([]config.Unholdable, 0, len(e.unhold))
	for _, u := range e.unhold {
		list = append(list, u)
	}
	if err := e.dirs.SaveUnholdable(list); err != nil {
		slog.Warn("cannot save the unsyncable-name list", "err", err)
	}
}

// SetUnholdable records what one listing of the server folder parent found:
// its children whose names this OS cannot hold. It replaces whatever was
// recorded for that folder before, so an item renamed on the server (by
// anyone) drops off the list the next time its folder is listed. localDir
// and remoteRoot are the sync folder the listing belongs to.
//
// Called for every folder a pass lists, nearly always with nothing to record
// and nothing recorded, so that case touches nothing.
func (e *Engine) SetUnholdable(localDir, remoteRoot, parent string, items []engine.Unholdable) {
	parent = strings.Trim(parent, "/")
	e.unholdMu.Lock()
	e.loadUnholdLocked()
	want := make(map[string]bool, len(items))
	var added []string
	changed := false
	for _, it := range items {
		r := strings.Trim(it.Remote, "/")
		want[r] = true
		if _, ok := e.unhold[r]; ok {
			continue
		}
		e.unhold[r] = config.Unholdable{LocalDir: localDir, RemoteRoot: strings.Trim(remoteRoot, "/"), Remote: r, IsDir: it.IsDir}
		added = append(added, r)
		changed = true
	}
	for r, u := range e.unhold {
		if remoteParent(r) == parent && !want[r] && config.PathKey(u.LocalDir) == config.PathKey(localDir) {
			delete(e.unhold, r)
			changed = true
		}
	}
	if changed {
		e.saveUnholdLocked()
	}
	e.unholdMu.Unlock()
	if !changed {
		return
	}
	e.notifyBlockedSubs()
	for _, r := range added {
		slog.Info("cannot sync a server item: Windows cannot hold its name", "path", r)
		e.toast("Can't sync "+path.Base(r), UnholdableReason, "")
	}
}

// unholdableFor is the RemoteScan hook for a live-mode pair.
func (e *Engine) unholdableFor(p Pair) func(string, []engine.Unholdable) {
	return func(dir string, items []engine.Unholdable) {
		e.SetUnholdable(p.LocalDir, p.RemoteRoot, dir, items)
	}
}

// unholdableFiles returns the recorded items as "Can't sync" rows.
func (e *Engine) unholdableFiles() []BlockedFile {
	e.unholdMu.Lock()
	defer e.unholdMu.Unlock()
	e.loadUnholdLocked()
	out := make([]BlockedFile, 0, len(e.unhold))
	for _, u := range e.unhold {
		rel := strings.TrimPrefix(strings.TrimPrefix(u.Remote, u.RemoteRoot), "/")
		out = append(out, BlockedFile{
			LocalDir: u.LocalDir,
			Path:     rel,
			Abs:      filepath.Join(u.LocalDir, filepath.FromSlash(rel)),
			Reason:   UnholdableReason,
			IsDir:    u.IsDir,
			Remote:   u.Remote,
		})
	}
	return out
}

// RenameOnServer renames a recorded server item (remote, files-root-relative)
// to newName in the same server folder, never replacing anything already
// there, then drops it from "Can't sync" and asks for a sync so it comes down.
// The error text is meant for the user.
func (e *Engine) RenameOnServer(remote, newName string) error {
	remote = strings.Trim(remote, "/")
	switch {
	case strings.TrimSpace(newName) == "":
		return errors.New("enter a name")
	case strings.ContainsAny(newName, `/\`):
		return errors.New("a name can't contain / or \\")
	case winname.CannotHold(newName):
		return errors.New("the new name must not end in a space or a full stop")
	}
	dst := newName
	if p := remoteParent(remote); p != "" {
		dst = p + "/" + newName
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := e.MoveRemoteNoReplace(ctx, remote, dst)
	switch {
	case errors.Is(err, transport.ErrDestinationExists):
		return fmt.Errorf("%q already exists in that folder; pick another name", newName)
	case isNotFound(err):
		// Renamed or deleted by someone else meanwhile: nothing left to fix.
		e.dropUnholdable(remote)
		e.TriggerSync()
		return nil
	case err != nil:
		return err
	}
	e.dropUnholdable(remote)
	e.TriggerSync()
	return nil
}

// isNotFound reports a 404 from the server.
func isNotFound(err error) bool {
	var se *transport.StatusError
	return errors.As(err, &se) && se.Code == 404
}

// dropUnholdable removes one recorded item and tells the subscribers.
func (e *Engine) dropUnholdable(remote string) {
	e.unholdMu.Lock()
	e.loadUnholdLocked()
	_, ok := e.unhold[remote]
	if ok {
		delete(e.unhold, remote)
		e.saveUnholdLocked()
	}
	e.unholdMu.Unlock()
	if ok {
		e.notifyBlockedSubs()
	}
}

// notifyBlockedSubs signals every "Can't sync" subscriber.
func (e *Engine) notifyBlockedSubs() {
	e.blockedMu.Lock()
	subs := append([]chan struct{}(nil), e.blockedSubs...)
	e.blockedMu.Unlock()
	notifyAll(subs)
}
