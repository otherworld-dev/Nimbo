package agent

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/otherworld/nimbo/internal/transfer"
)

// An upload is put off (transfer.InUseError) while another program still has
// the file open that was caught writing to it mid-upload: Outlook with an
// attached .pst (Deck #691). Two things follow from that here.
//
// The pass is not "Up to date": waitingStatus says what it is waiting for.
//
// And something has to try again once the program lets go. Closing a file need
// not write to it, so need not raise a watcher event, and without a nudge the
// upload waited for the hourly full pass. awaitClosed checks the file every
// closedCheckEvery, holding nothing, and nudges the path into a quick sync once
// no program has it open to write.

var (
	// writerGone reports whether nothing any longer holds abs open to write, so
	// the put-off upload would go ahead. A variable so tests can drive it.
	writerGone = func(abs string) bool { return transfer.UploadDeferred(abs) == nil }
	// closedCheckEvery paces awaitClosed: one open attempt per file, cheap.
	closedCheckEvery = 30 * time.Second
)

// waitingStatus is the pass's status line when uploads are held back: held are
// files locked on the server by someone else, busy are files open locally in
// another program. With neither, the pass is up to date.
func waitingStatus(held, busy []string) string {
	switch {
	case len(held) == 0 && len(busy) == 0:
		return "Up to date"
	case len(busy) == 0:
		return heldStatus(held)
	case len(held) > 0:
		return fmt.Sprintf("Waiting — %d files are in use", len(held)+len(busy))
	case len(busy) == 1:
		return "Waiting — " + filepath.Base(filepath.FromSlash(busy[0])) + " is open in another program"
	default:
		return fmt.Sprintf("Waiting — %d files are open in other programs", len(busy))
	}
}

// awaitClosed watches abs, whose upload was put off, until no program holds it
// open to write, then nudges it into a quick sync of pair p. Once per path at
// a time; it ends with the engine.
func (e *Engine) awaitClosed(p Pair, abs string) {
	e.watchMu.Lock()
	ctx := e.runCtx
	if ctx == nil {
		e.watchMu.Unlock()
		return
	}
	if e.awaiting == nil {
		e.awaiting = make(map[string]bool)
	}
	if e.awaiting[abs] {
		e.watchMu.Unlock()
		return
	}
	e.awaiting[abs] = true
	e.watchMu.Unlock()

	go func() {
		defer func() {
			e.watchMu.Lock()
			delete(e.awaiting, abs)
			e.watchMu.Unlock()
		}()
		t := time.NewTicker(closedCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if !writerGone(abs) {
				continue
			}
			e.watchMu.Lock()
			ch := e.nudges[PairKey(p.LocalDir, p.RemoteRoot)]
			e.watchMu.Unlock()
			if ch == nil {
				return // the pair's watcher is gone; its next start syncs everything
			}
			select {
			case ch <- abs:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// movedAsideToast words the one notification a pass raises for everything it
// moved next to the sync folder because the Recycle Bin couldn't take it (see
// transfer.Executor.removeMirrored). One per pass: a folder deleted on the
// server arrives file by file, and a toast each was thousands of toasts.
func movedAsideToast(root string, rels []string) (title, msg string) {
	clean := filepath.Clean(root)
	aside := filepath.Join(filepath.Dir(clean), filepath.Base(clean)+" - removed on server")
	if len(rels) == 1 {
		name := filepath.Base(filepath.FromSlash(rels[0]))
		return "Kept a copy of " + name, "\u201c" + name + "\u201d was deleted on the server and is too big for the Recycle Bin, " +
			"so it was moved to " + aside + ". Delete it from there once you no longer need it."
	}
	return fmt.Sprintf("Kept %d items deleted on the server", len(rels)),
		"They were deleted on the server and the Recycle Bin couldn't take them, so they were moved to " +
			aside + ". Delete them from there once you no longer need them."
}
