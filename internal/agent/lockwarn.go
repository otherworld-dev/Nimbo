package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/officelock"
)

// lockWarner makes somebody else's lock visible to the local user's editor.
//
// Live sync drives it from applyPlan; on-demand mode from NoteRemoteLocks
// (Deck #721). It writes real files next to the document, and a real file in an
// on-demand folder the filter has not populated yet makes it look populated —
// the placeholders then never appear. That cannot happen here, because warn
// only acts on a document that is already downloaded, so its folder is
// populated; online-only documents are skipped (fileOnlineOnly). The watcher
// keeps these files off the server, and calls BeforeReplace (release, below)
// before it dehydrates a held file.
//
// Two halves, and BOTH are needed for Microsoft Office:
//
//   - a deny-write handle on the document, which is what actually makes Office
//     refuse to open it for editing;
//   - a synthesised owner file beside it, which is only how Office learns WHOSE
//     lock it is. On its own it produces no dialog at all.
//
// LibreOffice is the exception: it treats its own `.~lock.` file's presence as
// the lock, so for those formats the file alone is enough.
//
// Everything it creates is recorded on disk, because a synthesised "~$Report.docx"
// is indistinguishable from a real one and deleting somebody's genuine owner
// file would break their Office session.
type lockWarner struct {
	dirs config.Dirs
	user func() string // display name to write into the owner file

	mu    sync.Mutex
	held  map[string]*warnEntry // absolute document path -> what we did for it
	synth map[string]bool       // absolute paths of files WE created
}

type warnEntry struct {
	handle *denyWriteHandle
	files  []string // synthesised name carriers, absolute
	who    string
}

func newLockWarner(dirs config.Dirs, user func() string) *lockWarner {
	return &lockWarner{
		dirs: dirs, user: user,
		held:  map[string]*warnEntry{},
		synth: map[string]bool{},
	}
}

// apply reconciles the warnings against the set of files others hold locked.
// Anything newly locked gets warned about; anything no longer locked is undone.
func (w *lockWarner) apply(locked []LockedFile) {
	want := make(map[string]LockedFile, len(locked))
	for _, f := range locked {
		if f.Abs != "" {
			want[f.Abs] = f
		}
	}

	w.mu.Lock()
	var add []LockedFile
	var drop []string
	for abs, f := range want {
		if _, have := w.held[abs]; !have {
			add = append(add, f)
		}
	}
	for abs := range w.held {
		if _, still := want[abs]; !still {
			drop = append(drop, abs)
		}
	}
	w.mu.Unlock()

	for _, abs := range drop {
		w.clear(abs)
	}
	for _, f := range add {
		w.warn(f)
	}
}

// warn holds the document and writes the name carrier beside it.
func (w *lockWarner) warn(f LockedFile) {
	if _, err := os.Stat(f.Abs); err != nil {
		return // not downloaded here; nothing to protect
	}
	// An on-demand file whose data is not here: the handle's open would
	// download it, and an owner file without the handle stops nothing. Left
	// unrecorded, so a later apply picks it up once it has been downloaded.
	if fileOnlineOnly(f.Abs) {
		return
	}
	e := &warnEntry{who: f.Who()}

	// Name carrier first: if the handle succeeds, the editor may look for this
	// file immediately, and an unnamed lock reads worse than a named one.
	dir, name := filepath.Split(f.Abs)
	if body := officelock.OwnerFileContent(name, e.who); body != nil {
		if p, ok := w.writeSynth(filepath.Join(dir, officelock.OwnerFile(name))); ok {
			if err := os.WriteFile(p, body, 0o644); err == nil {
				e.files = append(e.files, p)
			} else {
				w.forgetSynth(p)
			}
		}
	}
	host, _ := os.Hostname()
	body := officelock.LibreLockContent(e.who, e.who, host, time.Now(), "")
	if p, ok := w.writeSynth(filepath.Join(dir, officelock.LibreLockFile(name))); ok {
		if err := os.WriteFile(p, []byte(body), 0o644); err == nil {
			e.files = append(e.files, p)
		} else {
			w.forgetSynth(p)
		}
	}

	// The handle is what actually stops Office. Failing is normal and quiet: it
	// usually means the local user already has the file open, in which case it
	// is already protected by them.
	if h, err := holdDenyWrite(f.Abs); err == nil {
		e.handle = h
	} else {
		slog.Debug("could not hold a locked file open", "path", f.Abs, "err", err)
	}

	w.mu.Lock()
	w.held[f.Abs] = e
	w.mu.Unlock()
	slog.Info("warning locally about a file someone else has open", "path", f.Abs, "by", e.who)
}

// clear undoes warn: release the handle, delete only what we created.
func (w *lockWarner) clear(abs string) {
	w.mu.Lock()
	e := w.held[abs]
	delete(w.held, abs)
	w.mu.Unlock()
	if e == nil {
		return
	}
	if e.handle != nil {
		_ = e.handle.Close()
	}
	for _, p := range e.files {
		_ = os.Remove(p)
		w.forgetSynth(p)
	}
	slog.Info("cleared the local warning", "path", abs)
}

// release drops the handle on one path WITHOUT forgetting the warning, so our
// own downloader can replace the file. Without this every sync of a file that
// somebody else has locked would fail against our own handle.
func (w *lockWarner) release(abs string) {
	w.mu.Lock()
	e := w.held[abs]
	w.mu.Unlock()
	if e == nil || e.handle == nil {
		return
	}
	_ = e.handle.Close()
	w.mu.Lock()
	e.handle = nil
	w.mu.Unlock()
}

// retake re-holds a path we released for a transfer, if it is still warned.
func (w *lockWarner) retake(abs string) {
	w.mu.Lock()
	e := w.held[abs]
	w.mu.Unlock()
	if e == nil || e.handle != nil {
		return
	}
	if h, err := holdDenyWrite(abs); err == nil {
		w.mu.Lock()
		e.handle = h
		w.mu.Unlock()
	}
}

// closeAll drops every handle and removes every file we made. Shutdown path.
func (w *lockWarner) closeAll() {
	w.mu.Lock()
	paths := make([]string, 0, len(w.held))
	for abs := range w.held {
		paths = append(paths, abs)
	}
	w.mu.Unlock()
	for _, abs := range paths {
		w.clear(abs)
	}
}

// sweep deletes name carriers a previous run left behind. Only files in our own
// record are touched — an unrecorded "~$Report.docx" belongs to a real Office
// session and removing it would break that session's locking.
func (w *lockWarner) sweep() int {
	left := w.dirs.LoadSynthFiles()
	n := 0
	for _, p := range left {
		if err := os.Remove(p); err == nil {
			n++
			slog.Info("removed a lock warning left by a previous run", "path", p)
		}
	}
	_ = w.dirs.SaveSynthFiles(nil)
	w.mu.Lock()
	w.synth = map[string]bool{}
	w.mu.Unlock()
	return n
}

// writeSynth records a path as ours BEFORE creating it, so a crash mid-write
// still leaves something the next sweep can clean up. ok is false when the path
// already exists and is not ours — a real Office owner file, which we must not
// overwrite or later delete.
func (w *lockWarner) writeSynth(path string) (string, bool) {
	w.mu.Lock()
	mine := w.synth[path]
	w.mu.Unlock()
	if !mine {
		if _, err := os.Stat(path); err == nil {
			slog.Debug("not overwriting an existing editor lock file", "path", path)
			return "", false
		}
	}
	w.mu.Lock()
	w.synth[path] = true
	list := w.synthListLocked()
	w.mu.Unlock()
	_ = w.dirs.SaveSynthFiles(list)
	return path, true
}

// isSynth reports whether path is an editor lock file WE wrote (compared
// without case: the change journal reports the name as Windows stored it).
func (w *lockWarner) isSynth(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.synth[path] {
		return true
	}
	for p := range w.synth {
		if strings.EqualFold(p, path) {
			return true
		}
	}
	return false
}

func (w *lockWarner) forgetSynth(path string) {
	w.mu.Lock()
	delete(w.synth, path)
	list := w.synthListLocked()
	w.mu.Unlock()
	_ = w.dirs.SaveSynthFiles(list)
}

func (w *lockWarner) synthListLocked() []string {
	out := make([]string, 0, len(w.synth))
	for p := range w.synth {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
