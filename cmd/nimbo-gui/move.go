package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/shellns"
)

// MoveSyncFolder re-points the sync folder at oldLocal to newLocal WITHOUT
// re-downloading. If newLocal already holds the content (the user moved it in
// Explorer / restored a backup) it just re-keys the sync state; otherwise it
// relocates the folder there first — an instant rename on the same drive, or a
// recursive copy-then-delete across drives. Returns "" on success, else a
// human-readable error.
func (a *App) MoveSyncFolder(oldLocal, newLocal string) string {
	if a.eng == nil {
		return "Not signed in."
	}
	oldLocal = filepath.Clean(strings.TrimSpace(oldLocal))
	newLocal = filepath.Clean(strings.TrimSpace(newLocal))
	if newLocal == "" || !filepath.IsAbs(newLocal) {
		return "Pick a full destination path."
	}
	if strings.EqualFold(oldLocal, newLocal) {
		return "" // same place — nothing to do
	}
	// v1: on-demand (virtual files) folders need cloud-files sync-root
	// re-registration, which isn't handled yet.
	if _, ok := a.onDemandMountFor(oldLocal); ok {
		return "Moving the folder isn't supported in on-demand (virtual files) mode yet."
	}

	// Hand the file move to the engine as a callback. It stops AND drains the
	// pair's watcher BEFORE this relocate runs, then re-keys + reloads — so the old
	// folder is never emptied while a sync could read it as mass deletions (the bug
	// that deleted server files). The relocate either moves the folder or confirms
	// the content is already at the new location.
	relocate := func() error {
		if msg := relocateFolder(oldLocal, newLocal); msg != "" {
			return errors.New(msg)
		}
		return nil
	}
	if err := a.eng.MoveSyncPair(oldLocal, newLocal, relocate); err != nil {
		return err.Error()
	}

	// The whole-account pair defines the account root: refresh the stored baseDir
	// and the Explorer sidebar so "Open folder" and the sidebar follow the move.
	a.healBaseDir()
	if shellns.Enabled() {
		if icon, err := navIconPath(); err == nil {
			_ = shellns.Register(brand.Current.Name, a.GetBaseDir(), icon)
		}
	}
	a.rebuildTrayMenu()
	a.eng.TriggerSync() // a confirming pass; should be a no-op (everything already in sync)
	return ""
}

// relocateFolder ensures src's content ends up at dst. If dst already exists with
// content, the user moved it themselves — nothing to do (re-point). Otherwise the
// folder is moved: a rename when possible, else a recursive copy + delete (for a
// cross-volume move). File mtimes are preserved so the moved files don't read as
// edits against the sync baseline. Returns "" on success, else an error message.
func relocateFolder(src, dst string) string {
	// Refuse a destination that already has files. Nimbo must do the move itself
	// (with the watcher stopped) — letting the user pre-move the folder in Explorer
	// while Nimbo is watching is exactly what propagates deletions to the server.
	if di, err := os.Stat(dst); err == nil && di.IsDir() && !folderEmpty(dst) {
		return "That folder already has files in it. Pick a new or empty location and Nimbo will move your synced files there.\n\nDon't move the folder yourself in Explorer while Nimbo is running — that's what can delete files on the server."
	}
	si, serr := os.Stat(src)
	if serr != nil || !si.IsDir() {
		return "The current sync folder is missing — can't move it."
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "Couldn't prepare the destination: " + err.Error()
	}
	// Clear an empty (or stray-file) dst so Rename can create it.
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			return "The destination is in the way: " + err.Error()
		}
	}
	if err := os.Rename(src, dst); err == nil {
		return "" // fast path: same volume, instant, mtimes preserved
	}
	// Cross-volume (or rename refused): copy, VERIFY, then delete the original — so
	// the source is never removed until the copy is proven complete and intact.
	// Pre-check free space first so we don't start a doomed copy that fills the disk.
	if need := dirSize(src); need > 0 {
		if free := diskFree(dst); free > 0 && free < need {
			return fmt.Sprintf("Not enough space at the destination — about %d MB is needed but only %d MB is free. Free up space or pick another drive. Nothing has been moved.", need/(1<<20)+1, free/(1<<20))
		}
	}
	if err := copyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst) // don't leave a partial copy behind
		return "Couldn't copy the folder to the new location: " + err.Error() + "\n\nYour original is untouched — nothing was deleted."
	}
	if err := verifyCopy(src, dst); err != nil {
		_ = os.RemoveAll(dst) // the copy is incomplete/corrupt — discard it, keep the source
		return "The copy didn't verify (" + err.Error() + "). Your original is untouched and nothing was deleted — please try again."
	}
	_ = os.RemoveAll(src) // copy verified complete; remove the original (sync follows dst)
	return ""
}

// dirSize returns the total size in bytes of the files under dir (0 if unreadable).
func dirSize(dir string) uint64 {
	var total uint64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil && info.Size() > 0 {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}

// verifyCopy confirms dst is a complete, intact copy of src: every file and
// directory under src exists under dst, and every file's size matches. It is the
// gate a cross-volume move must pass BEFORE deleting the source. Because dst
// starts empty, a full match means the trees are equal.
func verifyCopy(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		ti, terr := os.Stat(filepath.Join(dst, rel))
		if terr != nil {
			return fmt.Errorf("%s missing from the copy", rel)
		}
		if d.IsDir() {
			if !ti.IsDir() {
				return fmt.Errorf("%s should be a folder in the copy", rel)
			}
			return nil
		}
		si, serr := d.Info()
		if serr != nil {
			return serr
		}
		if ti.Size() != si.Size() {
			return fmt.Errorf("%s is %d bytes, expected %d", rel, ti.Size(), si.Size())
		}
		return nil
	})
}

// copyTree recursively copies src into dst, preserving file mtimes so the moved
// files keep matching the sync baseline rather than looking modified.
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
			mode := fs.FileMode(0o755)
			if info, ierr := d.Info(); ierr == nil {
				mode = info.Mode().Perm()
			}
			return os.MkdirAll(target, mode)
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if fi, err := os.Stat(src); err == nil {
		_ = os.Chmod(dst, fi.Mode().Perm())
		_ = os.Chtimes(dst, fi.ModTime(), fi.ModTime()) // preserve mtime for the baseline
	}
	return nil
}

func folderEmpty(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return err == io.EOF
}
