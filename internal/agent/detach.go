package agent

// Keeping the local copy of a share or mount that has been DETACHED from the
// account (Deck #557): a folder someone stopped sharing with you, a group
// folder you were removed from, an external storage the admin unmounted.
//
// The rule that tells an unshare from a deletion lives in engine.KeepDetached
// (live mode) and in the on-demand watcher's reconcile (vfs). This is the
// wiring around both: the copy is PARKED — recorded in the account's detached
// list, which every live pass excludes exactly — and the user is told. Nothing
// is uploaded, moved or deleted until they choose (ResolveDetached): keep it as
// their own, move it out of the sync folder, or delete it.
//
// A live-mode copy stays where it is. An on-demand copy cannot: a cloud sync
// root only shows truthful state for things the server has, so its salvaged
// copy is moved beside the sync folder (ParkDetachedCopy) and "keep" moves it
// back in.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/activity"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transfer"
	"github.com/otherworld/nimbo/internal/transport"
)

// ignoreFor builds a pass's ignore matcher: the global patterns, the pair's
// selective-sync excludes, and — exactly, by path — every folder parked in
// place in this pair. It is the one place those are combined, so no sync route
// can forget the parked ones and upload a copy the user has not decided about.
func (e *Engine) ignoreFor(p Pair) *engine.Ignore {
	globalIgnore, _ := e.dirs.LoadIgnore()
	ig := engine.NewIgnore(append(append([]string{}, globalIgnore...), p.Excludes...))
	list, err := e.dirs.LoadDetached()
	if err != nil {
		slog.Warn("cannot read the parked-folder list; a parked folder may sync this pass", "err", err)
		return ig
	}
	for _, d := range list {
		if d.ParkedAt == "" && samePair(d.LocalDir, d.RemoteRoot, p) {
			ig.AddExact(d.Rel)
		}
	}
	return ig
}

func samePair(localDir, remoteRoot string, p Pair) bool {
	return config.PathKey(localDir) == config.PathKey(p.LocalDir) &&
		strings.Trim(remoteRoot, "/") == strings.Trim(p.RemoteRoot, "/")
}

// keepDetached rewrites a live-mode plan so a detached share is kept, and parks
// every root it kept. A plan with no local delete is returned untouched without
// reading anything.
func (e *Engine) keepDetached(st *state.Store, pk string, p Pair, actions []engine.Action, base map[string]engine.BaselineState) ([]engine.Action, error) {
	deletes := false
	for _, a := range actions {
		if a.Kind == engine.ActDeleteLocal {
			deletes = true
			break
		}
	}
	if !deletes {
		return actions, nil
	}
	// SyncPaths hands applyPlan a nil base on purpose (see baselineForGuard);
	// the rows for just this plan's paths are enough to recognise a root.
	gbase, err := e.baselineForGuard(st, pk, base, actions)
	if err != nil {
		return nil, err
	}
	out, detached := engine.KeepDetached(actions, gbase)
	if len(detached) == 0 {
		return actions, nil
	}
	var parked []string
	for _, root := range detached {
		// Park it FIRST. The exclusion is what keeps the copy out of sync, so it
		// must be on disk before the rows go; if parking fails the rows stay,
		// the next pass plans the same deletes, and this runs again. Either way
		// this pass no longer touches the folder.
		if err := e.dirs.AddDetached(config.Detached{
			LocalDir: p.LocalDir, RemoteRoot: p.RemoteRoot, Rel: root, AtUnix: time.Now().Unix(),
		}); err != nil {
			slog.Warn("could not park a detached share; will retry next pass", "path", root, "err", err)
			continue
		}
		// Forget the subtree: with the rows gone nothing under it can ever again
		// read as "removed remotely". Harmless if it fails — a parked folder is
		// filtered from both sides, so the dead-row prune drops them anyway.
		if err := st.DeleteBaselineUnder(pk, root); err != nil {
			slog.Warn("could not forget a detached share's baseline rows", "path", root, "err", err)
		}
		slog.Warn("no longer shared with you (or unmounted): keeping the local copy, parked until you decide",
			"dir", p.LocalDir, "path", root)
		e.recorder.Add(activity.Event{Local: p.LocalDir, Path: root, Kind: "unshared"})
		parked = append(parked, root)
	}
	if len(parked) > 0 {
		e.notifyDetached()
		e.toastDetached(parked, "")
	}
	return out, nil
}

// ParkDetachedCopy is the on-demand counterpart of keepDetached. The watcher
// has already salvaged the share's bytes at from (plain files now); this moves
// the folder beside the sync folder — a cloud root cannot hold a copy of
// something the server no longer has — records it as parked, and tells the
// user. It returns where the copy went. A copy that was moved but could not be
// recorded still returns its path (with the error): the files are safe outside
// the sync folder, and the log names them.
func (e *Engine) ParkDetachedCopy(localDir, remoteRoot, rel, from string) (string, error) {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	dir := e.parkingDir(localDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %q: %w", dir, err)
	}
	target := freeName(filepath.Join(dir, path.Base(rel)))
	if err := moveTree(from, target); err != nil {
		return "", err
	}
	if err := e.dirs.AddDetached(config.Detached{
		LocalDir: localDir, RemoteRoot: remoteRoot, Rel: rel, AtUnix: time.Now().Unix(), ParkedAt: target,
	}); err != nil {
		slog.Error("no longer shared with you: your copy was moved but could not be recorded — it is safe here",
			"path", rel, "movedTo", target, "err", err)
		return target, err
	}
	slog.Warn("no longer shared with you (or unmounted): kept your copy beside the sync folder, parked until you decide",
		"dir", localDir, "path", rel, "movedTo", target)
	e.notifyDetached()
	e.toastDetached([]string{rel}, dir)
	return target, nil
}

// parkingDir is where an on-demand folder's detached copies go: a sibling of
// the sync folder named after it, so it is found next to the thing it came
// from. It must not sit inside any synced folder (a sync folder at a drive
// root would otherwise get its "sibling" inside itself); then the user's home
// directory takes it.
func (e *Engine) parkingDir(localDir string) string {
	clean := filepath.Clean(localDir)
	base := filepath.Base(clean)
	if base == "." || base == string(filepath.Separator) || base == filepath.VolumeName(clean) {
		base = "Sync folder"
	}
	name := base + " - no longer shared"
	candidate := filepath.Join(filepath.Dir(clean), name)
	if !e.insideSyncFolder(candidate, localDir) {
		return candidate
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return candidate
	}
	return filepath.Join(home, name)
}

// freeName returns target, or the first "target (n)" that does not exist yet:
// a second unshare of a folder with the same name must not overwrite the copy
// already parked under it.
func freeName(target string) string {
	if _, err := os.Lstat(target); err != nil {
		return target
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s (%d)", target, i)
		if _, err := os.Lstat(c); err != nil {
			return c
		}
	}
}

// toastDetached tells the user a share or mount has gone from their account
// and that their copy is waiting for a decision — an outcome they would
// otherwise only discover by noticing the folder never updates again. movedTo
// names the parking folder for an on-demand copy; empty for one kept in place.
// Not throttled with the error toasts: it is rare, and it is not an error.
func (e *Engine) toastDetached(roots []string, movedTo string) {
	if e.onToast == nil || len(roots) == 0 {
		return
	}
	names := make([]string, len(roots))
	for i, r := range roots {
		names[i] = "“" + path.Base(r) + "”"
	}
	what, verb := names[0], "is"
	switch {
	case len(names) == 2:
		what, verb = names[0]+" and "+names[1], "are"
	case len(names) > 2:
		what, verb = names[0]+", "+names[1]+" and "+strconv.Itoa(len(names)-2)+" more", "are"
	}
	tail := "your copy has been kept and is not syncing. Choose what to do with it."
	if movedTo != "" {
		tail = "your copy was moved to “" + movedTo + "” and is not syncing. Choose what to do with it."
	}
	e.toast("Nimbo — no longer shared with you",
		what+" "+verb+" no longer shared with you (or the storage was unmounted). Nothing was deleted: "+tail,
		"action=detached")
}

// DetachedFolder is a parked copy, for the UI.
type DetachedFolder struct {
	LocalDir   string
	RemoteRoot string
	Rel        string    // pair-relative
	Name       string    // its folder name, for display
	LocalPath  string    // where the kept copy is — the key ResolveDetached takes
	MovedOut   bool      // it sits beside the sync folder (on-demand), not inside it
	At         time.Time // when it was parked
}

// DetachedFolders lists the parked copies awaiting a decision.
func (e *Engine) DetachedFolders() []DetachedFolder {
	list, err := e.dirs.LoadDetached()
	if err != nil {
		slog.Warn("cannot read the parked-folder list", "err", err)
		return nil
	}
	out := make([]DetachedFolder, 0, len(list))
	for _, d := range list {
		out = append(out, DetachedFolder{
			LocalDir: d.LocalDir, RemoteRoot: d.RemoteRoot, Rel: d.Rel,
			Name:      path.Base(d.Rel),
			LocalPath: d.LocalPath(),
			MovedOut:  d.ParkedAt != "",
			At:        time.Unix(d.AtUnix, 0),
		})
	}
	return out
}

// SubscribeDetached returns a channel signalled whenever the parked list
// changes, so a UI can refresh.
func (e *Engine) SubscribeDetached() <-chan struct{} {
	ch := make(chan struct{}, 1)
	e.detachedMu.Lock()
	e.detachedSubs = append(e.detachedSubs, ch)
	e.detachedMu.Unlock()
	return ch
}

func (e *Engine) notifyDetached() {
	e.detachedMu.Lock()
	subs := append([]chan struct{}(nil), e.detachedSubs...)
	e.detachedMu.Unlock()
	notifyAll(subs)
}

// ResolveDetached carries out the user's decision for the parked copy at
// localPath (DetachedFolder.LocalPath):
//
//   - "keep": it becomes the user's own — un-parked, it is new local content
//     and syncs to their account like any other folder. A copy parked beside
//     an on-demand folder is first moved back in;
//   - "delete": the copy goes to the Recycle Bin (where there is one);
//   - "move": the folder is moved into dest, a directory outside every sync
//     folder, under its own name.
//
// For delete and move the copy leaves the sync folder BEFORE it is un-parked,
// so no pass in between can read it as new content and upload it.
func (e *Engine) ResolveDetached(localPath, choice, dest string) error {
	list, err := e.dirs.LoadDetached()
	if err != nil {
		return err
	}
	var entry *config.Detached
	for i := range list {
		if config.PathKey(list[i].LocalPath()) == config.PathKey(localPath) {
			entry = &list[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("%q is not waiting for a decision", localPath)
	}
	src := entry.LocalPath()
	switch choice {
	case "keep":
		if entry.ParkedAt != "" {
			// Back into the sync folder, where it syncs as the user's own. Never
			// over something that is there now: the folder may have been shared
			// with them again, and merging is not ours to decide.
			target := filepath.Join(entry.LocalDir, filepath.FromSlash(entry.Rel))
			if _, err := os.Lstat(target); err == nil {
				return fmt.Errorf("%q exists in your sync folder again (shared with you again?) — move or delete your copy instead, or rename it first", target)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := moveTree(src, target); err != nil {
				return err
			}
		}
	case "delete":
		if err := transfer.RemoveToBin(src); err != nil {
			return fmt.Errorf("remove %q: %w", src, err)
		}
	case "move":
		target, err := e.moveTarget(entry.LocalDir, src, dest)
		if err != nil {
			return err
		}
		if err := moveTree(src, target); err != nil {
			return err
		}
		slog.Info("moved a parked folder out", "from", src, "to", target)
	default:
		return fmt.Errorf("unknown choice %q", choice)
	}
	if err := e.dirs.RemoveDetached(src); err != nil {
		return err
	}
	if entry.ParkedAt != "" {
		_ = os.Remove(filepath.Dir(entry.ParkedAt)) // an emptied parking folder; stays if anything else is in it
	}
	slog.Info("parked folder resolved", "path", entry.Rel, "choice", choice)
	e.notifyDetached()
	e.TriggerSync()
	return nil
}

// moveTarget validates a move destination and returns the folder's new path.
// It must be an existing directory outside every sync folder (a copy moved
// into one would sync from there as new content), and the new path must be
// free — a move never overwrites.
func (e *Engine) moveTarget(pairDir, src, dest string) (string, error) {
	if dest == "" {
		return "", errors.New("choose a folder to move it to")
	}
	if fi, err := os.Stat(dest); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%q is not a folder", dest)
	}
	if e.insideSyncFolder(dest, pairDir) {
		return "", fmt.Errorf("%q is inside a synced folder — choose somewhere outside your sync folders", dest)
	}
	target := filepath.Join(dest, filepath.Base(src))
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%q already exists — move it aside first", target)
	}
	return target, nil
}

// insideSyncFolder reports whether p is (or lies beneath) localDir or any
// configured sync folder.
func (e *Engine) insideSyncFolder(p, localDir string) bool {
	roots := []string{localDir}
	if pairs, err := e.Pairs(); err == nil {
		for _, sp := range pairs {
			roots = append(roots, sp.LocalDir)
		}
	}
	for _, r := range roots {
		if within(p, r) {
			return true
		}
	}
	return false
}

// within reports whether p is root or lies beneath it, comparing the way the
// rest of config compares local paths.
func within(p, root string) bool {
	pk, rk := config.PathKey(p), config.PathKey(root)
	return pk == rk || strings.HasPrefix(pk, strings.TrimRight(rk, `\/`)+string(filepath.Separator))
}

// moveTree moves a directory: a rename where the filesystem allows it, and a
// copy followed by removal across volumes. The source of a copied move goes to
// the Recycle Bin rather than straight to deletion, so a bad copy is
// recoverable.
func moveTree(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst) // dst did not exist before (the caller checked), so this only removes the partial copy
		return fmt.Errorf("copy %q to %q: %w", src, dst, err)
	}
	return transfer.RemoveToBin(src)
}

// copyTree copies a directory tree (or a single file), keeping file
// modification times.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := copyFile(p, target); err != nil {
			return err
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// listingSelf reads whether a listing's own directory sits on a share or mount,
// from its own row. Nextcloud sends that row; absent, the answer is unknown —
// and unknown never marks a child as a root.
func listingSelf(entries []transport.Entry, dir string) (onMount, known bool) {
	for _, en := range entries {
		if strings.Trim(en.Path, "/") == dir {
			return en.OnMount(), true
		}
	}
	return false, false
}

// cloneRemoteState is the initial clone's Entry→RemoteState conversion, with
// MountRoot derived exactly as RemoteScan derives it: on a mount, parent known
// and not. The clone lists with its own PROPFINDs rather than RemoteScan, so
// without this the very first baseline rows of a share would carry no flag.
func cloneRemoteState(rel string, en transport.Entry, parentOnMount, parentKnown bool) engine.RemoteState {
	return engine.RemoteState{
		Path: rel, IsDir: en.IsDir, ETag: en.ETag, FileID: en.FileID, Size: en.Size, LastModified: en.LastModified,
		MountRoot: en.OnMount() && parentKnown && !parentOnMount,
	}
}
