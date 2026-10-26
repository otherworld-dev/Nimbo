//go:build windows

package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// Deck #984 (GitHub #7 reporter's log, 2026-10-26): a folder renamed on the
// server only in case, and a server folder whose name ends in a space.

// mkdirRaw creates a directory whose name Win32 would otherwise rewrite (a
// trailing space or dot) by going through the \\?\ prefix, exactly as the
// cloud filter does when it creates a placeholder by that name.
func mkdirRaw(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(`\\?\`+path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// dirNames lists a directory's entries by their exact on-disk names.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// A folder renamed on the server in case only ("14 days" -> "14 Days") still
// matches the local folder case-insensitively. It must then be listed by the
// SERVER's spelling — the old one is a 404 on every pass — and the local
// folder takes the new spelling, with every identity beneath it repointed so
// online-only files still open.
func TestReconcilePullsCaseOnlyFolderRename(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	old := filepath.Join(root, "Film - 14 days")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(old, "a.txt")
	if err := os.WriteFile(doc, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(old, "Film - 14 days")
	f.setIdentity(doc, "Film - 14 days/a.txt")

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Film - 14 Days", true, "e-dir", "")}
	a := ph("a.txt", false, "e-a", "f-a")
	a.Identity = []byte("Film - 14 Days/a.txt")
	rec.listing["Film - 14 Days"] = []cfapi.PlaceholderInfo{a}
	rec.listErrs["Film - 14 days"] = errors.New("404 not found") // the old spelling is gone
	rec.baselines["Film - 14 days/a.txt"] = "e-a"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()
	w.Reconcile() // the folder's ETag is now recorded: the subtree is skipped

	if got := dirNames(t, root); len(got) != 1 || got[0] != "Film - 14 Days" {
		t.Errorf("local root = %q, want the folder renamed to the server's spelling", got)
	}
	rec.mu.Lock()
	newCalls, oldCalls := rec.listCalls["Film - 14 Days"], rec.listCalls["Film - 14 days"]
	moves, deletes, uploads := len(rec.moves), len(rec.deletes), len(rec.uploads)
	rec.mu.Unlock()
	if oldCalls != 0 {
		t.Errorf("listed the old spelling %d time(s); it is a 404 on the server", oldCalls)
	}
	if newCalls != 1 {
		t.Errorf("listed the server spelling %d time(s), want 1 (then the ETag skip)", newCalls)
	}
	if id, _ := f.identityOf(filepath.Join(root, "Film - 14 Days", "a.txt")); id != "Film - 14 Days/a.txt" {
		t.Errorf("child identity = %q, want it repointed to the server spelling", id)
	}
	if id, _ := f.identityOf(filepath.Join(root, "Film - 14 Days")); id != "Film - 14 Days" {
		t.Errorf("folder identity = %q, want the server spelling", id)
	}
	if moves+deletes+uploads != 0 {
		t.Errorf("server ops for a rename the server already made: %d moves, %d deletes, %d uploads", moves, deletes, uploads)
	}
	if rec.baselines["Film - 14 Days/a.txt"] != "e-a" {
		t.Errorf("child baseline not carried to the new spelling: %v", rec.baselines)
	}
}

// One folder that fails to list on every pass must not make every pass a full
// sweep of the whole account (the reporter's log: ~7,000 PROPFINDs an hour for
// a day, every change waiting up to an hour for the walk to reach it). The
// failing folder keeps being retried; everything else goes back to the cheap
// ETag skip once the first sweep has been everywhere.
func TestReconcileOneFailingFolderDoesNotKeepEveryPassFull(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	for _, d := range []string{"good", "bad"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "x.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("good", true, "e-good", ""), ph("bad", true, "e-bad", "")}
	rec.listing["good"] = []cfapi.PlaceholderInfo{ph("x.txt", false, "e-x", "f-x")}
	rec.listErrs["bad"] = errors.New("404 not found")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile() // the first, full sweep
	w.Reconcile()
	w.Reconcile()

	rec.mu.Lock()
	good, bad := rec.listCalls["good"], rec.listCalls["bad"]
	rec.mu.Unlock()
	if good != 1 {
		t.Errorf("unchanged folder listed %d times over 3 passes, want 1: one failing folder kept every pass full", good)
	}
	if bad != 3 {
		t.Errorf("failing folder listed %d times, want 3 (retried every pass)", bad)
	}

	// Once it lists again it is reconciled and joins the skip.
	rec.mu.Lock()
	delete(rec.listErrs, "bad")
	rec.listing["bad"] = []cfapi.PlaceholderInfo{ph("x.txt", false, "e-x2", "f-x2")}
	rec.mu.Unlock()
	w.Reconcile()
	w.Reconcile()
	rec.mu.Lock()
	bad = rec.listCalls["bad"]
	rec.mu.Unlock()
	if bad != 4 {
		t.Errorf("recovered folder listed %d times in total, want 4 (once more, then skipped)", bad)
	}
}

// A folder the first sweep could not reconcile is still owed that sweep's
// thoroughness: when it lists again, the pass that reaches it must not trust
// the ETag skip below it, or a #580-flattened file in there is never healed.
func TestReconcileFailedFolderGetsItsFullSweepLater(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	inner := filepath.Join(root, "bad", "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(inner, "doc.txt")
	if err := os.WriteFile(doc, []byte("same bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(doc) // flattened: only a full sweep's listing can heal it
	fi, _ := os.Stat(doc)
	r := ph("doc.txt", false, "e-doc", "f-doc")
	r.Identity = []byte("bad/inner/doc.txt")
	r.Size, r.ModTime = fi.Size(), fi.ModTime()

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("bad", true, "e-bad", "")}
	in := ph("inner", true, "e-inner", "")
	in.Identity = []byte("bad/inner")
	rec.listing["bad"] = []cfapi.PlaceholderInfo{in}
	rec.listing["bad/inner"] = []cfapi.PlaceholderInfo{r}
	rec.baselines["bad/inner"] = "e-inner" // unchanged: a normal pass would skip it
	rec.baselines["bad/inner/doc.txt"] = "e-doc"
	rec.listErrs["bad"] = errors.New("503")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile() // full sweep, "bad" fails
	rec.mu.Lock()
	delete(rec.listErrs, "bad")
	rec.mu.Unlock()
	w.Reconcile() // "bad" lists: its subtree is swept in full

	rec.mu.Lock()
	innerCalls := rec.listCalls["bad/inner"]
	rec.mu.Unlock()
	if innerCalls != 1 {
		t.Errorf("bad/inner listed %d times, want 1: the owed sweep trusted the ETag skip", innerCalls)
	}
	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	if !containsPath(marked, doc) {
		t.Error("flattened file under the once-failed folder was not healed")
	}
}

// A change pushed while a pass is running must not be lost: the poke that
// arrives mid-pass gets a pass of its own as soon as the current one ends.
func TestReconcileDuringAPassQueuesAnother(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	gate := make(chan struct{})
	rec.listGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	done := make(chan struct{})
	go func() { w.Reconcile(); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec.mu.Lock()
		n := rec.listCalls[""]
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first pass never listed the root")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Reconcile() // a push arrives mid-pass: must not be dropped
	rec.mu.Lock()
	rec.listGate = nil
	rec.mu.Unlock()
	close(gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pass never finished")
	}
	rec.mu.Lock()
	n := rec.listCalls[""]
	rec.mu.Unlock()
	if n != 2 {
		t.Errorf("root listed %d times, want 2: the mid-pass request was dropped", n)
	}
}

// --- names Windows cannot hold -----------------------------------------------

// The server can hold "Pilot Validation " (trailing space); Windows cannot
// address it — every Win32 path through it silently lands in "Pilot
// Validation". Reconcile must neither create it, nor put its children
// anywhere, nor read the real folder through the alias and MOVE it.
func TestReconcileSkipsServerNameEndingInSpace(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	pv := filepath.Join(root, "Pilot Validation")
	if err := os.MkdirAll(pv, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(pv, "doc.pdf")
	if err := os.WriteFile(doc, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(pv, "Pilot Validation")
	f.setIdentity(doc, "Pilot Validation/doc.pdf")

	rec := newRecorder()
	space := ph("Pilot Validation ", true, "e-space", "")
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Pilot Validation", true, "e-pv", ""), space}
	d := ph("doc.pdf", false, "e-doc", "f-doc")
	d.Identity = []byte("Pilot Validation/doc.pdf")
	rec.listing["Pilot Validation"] = []cfapi.PlaceholderInfo{d}
	n := ph("new.pdf", false, "e-new", "f-new")
	n.Identity = []byte("Pilot Validation /new.pdf")
	rec.listing["Pilot Validation "] = []cfapi.PlaceholderInfo{n}
	rec.baselines["Pilot Validation/doc.pdf"] = "e-doc"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()
	w.Reconcile()

	for _, c := range f.createdNames() {
		if c == "Pilot Validation " || c == "new.pdf" {
			t.Errorf("created %q: a name Windows cannot hold, or a child of one", c)
		}
	}
	if _, err := os.Stat(filepath.Join(pv, "new.pdf")); err == nil {
		t.Error("the space folder's child landed in the other folder")
	}
	assertNoServerOps(t, rec)
	rec.mu.Lock()
	got := append([][2]string(nil), rec.unholdable...)
	rec.mu.Unlock()
	if !containsPair(got, [2]string{"", "Pilot Validation "}) {
		t.Errorf("not reported to Can't sync: %v", got)
	}
	if !containsPair(got, [2]string{"Pilot Validation", ""}) {
		t.Errorf("a clean folder's listing must report too (so renamed items drop off): %v", got)
	}
}

// The reporter's PC already holds both folders, and the server has since
// renamed the space one away. The local alias must be left alone: reading it
// opens the REAL folder, whose identity then "mismatches" and is MOVEd onto
// the alias on the server (the "You renamed Pilot Validation to Pilot
// Validation" in their Activity), or deleted locally.
func TestReconcileLeavesALocalAliasFolderAlone(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	pv := filepath.Join(root, "Pilot Validation")
	if err := os.MkdirAll(pv, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(pv, "doc.pdf")
	if err := os.WriteFile(doc, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "Pilot Validation ")
	mkdirRaw(t, alias)
	f.setIdentity(pv, "Pilot Validation")
	f.setIdentity(doc, "Pilot Validation/doc.pdf")
	// What Windows really answers for the alias: any open through it reaches
	// the REAL folder, so its identity is the real folder's.
	f.setIdentity(alias, "Pilot Validation")

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Pilot Validation", true, "e-pv", ""), ph("Pilot Validationn", true, "e-n", "")}
	d := ph("doc.pdf", false, "e-doc", "f-doc")
	d.Identity = []byte("Pilot Validation/doc.pdf")
	rec.listing["Pilot Validation"] = []cfapi.PlaceholderInfo{d}
	rec.baselines["Pilot Validation/doc.pdf"] = "e-doc"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	assertNoServerOps(t, rec)
	if _, err := os.Stat(doc); err != nil {
		t.Errorf("the real folder's file is gone locally: %v", err)
	}
}

// Defence in depth: whatever puts a placeholder in the wrong folder through a
// Win32 alias, reconcile must not read the mismatch as the user's move. Two
// paths Windows treats as the same are not a move.
func TestReconcileDoesNotMoveAcrossATrailingSpaceAlias(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	pv := filepath.Join(root, "Pilot Validation")
	if err := os.MkdirAll(pv, 0o755); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(pv, "new.pdf")
	if err := os.WriteFile(stray, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(pv, "Pilot Validation")
	f.setIdentity(stray, "Pilot Validation /new.pdf") // the 11:35 misplacement

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Pilot Validation", true, "e-pv", "")}
	rec.listing["Pilot Validation"] = nil
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	rec.mu.Lock()
	moves := append([][2]string(nil), rec.moves...)
	rec.mu.Unlock()
	if len(moves) != 0 {
		t.Errorf("moved %v on the server: two names Windows treats as one are not a move", moves)
	}
}

func assertNoServerOps(t *testing.T, rec *recorder) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.moves)+len(rec.deletes)+len(rec.uploads)+len(rec.mkdirs) != 0 {
		t.Errorf("server ops: moves %v, deletes %v, uploads %v, mkdirs %v", rec.moves, rec.deletes, rec.uploads, rec.mkdirs)
	}
}

func containsPath(list []string, p string) bool {
	for _, x := range list {
		if strings.EqualFold(x, p) {
			return true
		}
	}
	return false
}

// A FILE renamed on the server in case only ("report.pdf" -> "Report.pdf").
// The listing still matches it, so before this nothing happened: an
// online-only stub kept opening the old, now-404 path, and an edit was PUT
// under the old spelling, creating a second file on the case-sensitive server.
func TestReconcilePullsCaseOnlyFileRename(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	old := filepath.Join(root, "report.pdf")
	if err := os.WriteFile(old, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(old, "report.pdf")
	f.markDehydrated(old)

	rec := newRecorder()
	r := ph("Report.pdf", false, "e-r", "f-r")
	rec.listing[""] = []cfapi.PlaceholderInfo{r}
	rec.baselines["report.pdf"] = "e-r"
	rec.fileids["report.pdf"] = "f-r"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if got := dirNames(t, root); len(got) != 1 || got[0] != "Report.pdf" {
		t.Errorf("local root = %q, want the file renamed to the server's spelling", got)
	}
	if id, _ := f.identityOf(filepath.Join(root, "Report.pdf")); id != "Report.pdf" {
		t.Errorf("identity = %q, want Report.pdf", id)
	}
	if rec.baselines["Report.pdf"] != "e-r" {
		t.Errorf("baseline not carried to the new spelling: %v", rec.baselines)
	}
	if rec.fileids["Report.pdf"] != "f-r" {
		t.Errorf("fileid not carried to the new spelling: %v", rec.fileids)
	}
	assertNoServerOps(t, rec)
}

// An edited file renamed on the server in case only keeps its edit and sends
// it under the NEW spelling: never a PUT to the old one.
func TestReconcileCaseOnlyFileRenameKeepsALocalEdit(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	old := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(old, []byte("edited here"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(old, "notes.txt")
	f.markDirty(old)
	f.markModified(old)
	osf := suppressFor
	suppressFor = 200 * time.Millisecond
	t.Cleanup(func() { suppressFor = osf })

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Notes.txt", false, "e-n", "f-n")}
	rec.baselines["notes.txt"] = "e-n"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	select {
	case got := <-rec.uploaded:
		if got != "Notes.txt" {
			t.Errorf("uploaded %q, want the edit sent as Notes.txt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the local edit was never uploaded")
	}
	// The repoint must keep the edit's state: MARK_IN_SYNC would let a
	// refresh or "free up space" dehydrate the only copy of it away.
	f.mu.Lock()
	keep := append([]string(nil), f.repointedKeep...)
	marked := append([]string(nil), f.repointed...)
	f.mu.Unlock()
	if !containsPath(keep, filepath.Join(root, "Notes.txt")) || containsPath(marked, filepath.Join(root, "Notes.txt")) {
		t.Errorf("edited file not repointed keep-state (keep=%v, in-sync=%v)", keep, marked)
	}
	rec.mu.Lock()
	ups := append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	for _, u := range ups {
		if u == "notes.txt" {
			t.Error("PUT to the old spelling: a second file on the case-sensitive server")
		}
	}
}

func containsPair(list [][2]string, p [2]string) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}
