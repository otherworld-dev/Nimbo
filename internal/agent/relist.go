package agent

// The one-off re-list (Deck #949).
//
// Builds before the fix to cloneRemote could leave a file out for good: a
// download that failed during a folder's first sync was never tried again,
// because the folders above it kept the etag their listing gave them and every
// later scan pruned them as unchanged. The fix stops new holes; it cannot find
// the old ones, since those folder rows are already stamped.
//
// So each sync folder that was first synced by an older build is listed in
// full once: its folder rows lose their etag (state.BlankDirETags), which is
// all it takes for the next remote scan to list every folder again, and
// anything on the server that is not on this PC is then downloaded like any
// other new file. The scan is the ordinary one, with its four listings at a
// time, its checkpoint (an interrupted crawl carries on where it stopped) and
// the failure backoff, the things put in after a crawl overloaded a server in
// July 2026. Two more limits here: one folder at a time across the engine, and
// once only, recorded per pair.

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transfer"
)

const (
	// relistMeta is the pair_meta name the re-list's progress is kept under:
	// "" (not done yet), relistRunning (folder rows blanked, the scan that
	// lists everything has not finished) or relistDone.
	relistMeta    = "relist-once"
	relistRunning = "running"
	relistDone    = "done"
)

// relistBegin starts, or carries on, a pair's one-off re-list at the top of a
// pass that will scan the pair's whole remote tree, and reports whether this
// pass is that re-list. The pair is past its first sync (the callers only get
// here when the clone status is "done").
//
// Only one pair re-lists at a time: another pair's turn comes on a later pass,
// once this one has finished.
func (e *Engine) relistBegin(st *state.Store, pk string, p Pair) bool {
	v, err := st.PairMeta(pk, relistMeta)
	if err != nil || v == relistDone {
		return false
	}
	e.relistMu.Lock()
	defer e.relistMu.Unlock()
	if e.relistActive != "" && e.relistActive != pk {
		return false
	}
	if v == relistRunning {
		// Blanked on an earlier pass or before a restart; the rows that pass
		// did not get to are still blank, so this pass lists them.
		e.relistActive = pk
		return true
	}
	n, err := st.BlankDirETags(pk)
	if err != nil {
		slog.Warn("one-off folder check: cannot start", "local", p.LocalDir, "err", err)
		return false
	}
	if err := st.SetPairMeta(pk, relistMeta, relistRunning); err != nil {
		slog.Warn("one-off folder check: cannot record the start", "local", p.LocalDir, "err", err)
	}
	e.relistActive = pk
	slog.Info("one-off folder check: listing every folder again", "local", p.LocalDir, "folders", n)
	e.toast(relistStartTitle, relistStartMessage(p.LocalDir), "")
	return true
}

// relistEnd closes the re-list once the pass that carried it has finished its
// scan and applied its plan. A pass that failed leaves it running: the folder
// rows it did not reconcile are still blank, and the next pass lists them.
func (e *Engine) relistEnd(st *state.Store, pk string, p Pair, relisting bool, stats transfer.Stats, err error) {
	if !relisting || err != nil {
		return
	}
	if merr := st.SetPairMeta(pk, relistMeta, relistDone); merr != nil {
		slog.Warn("one-off folder check: cannot record the end", "local", p.LocalDir, "err", merr)
		return
	}
	e.relistMu.Lock()
	if e.relistActive == pk {
		e.relistActive = ""
	}
	e.relistMu.Unlock()
	slog.Info("one-off folder check: finished", "local", p.LocalDir, "downloaded", stats.Downloaded, "failed", stats.Failed)
	e.toast(relistDoneTitle, relistDoneMessage(p.LocalDir, stats.Downloaded), "")
	// Another pair may be waiting its turn.
	e.TriggerSync()
}

const (
	relistStartTitle = "Checking your folders"
	relistDoneTitle  = "Folder check finished"
)

func relistStartMessage(localDir string) string {
	return fmt.Sprintf("An earlier version could leave a file out if its download failed during the first sync. "+
		"Nimbo is checking every folder in \"%s\" once to find any. You can keep working.", filepath.Base(localDir))
}

func relistDoneMessage(localDir string, downloaded int) string {
	name := filepath.Base(localDir)
	switch downloaded {
	case 0:
		return fmt.Sprintf("\"%s\": nothing was missing.", name)
	case 1:
		return fmt.Sprintf("\"%s\": 1 file that was not on this PC has been downloaded.", name)
	}
	return fmt.Sprintf("\"%s\": %d files that were not on this PC have been downloaded.", name, downloaded)
}
