package agent

import (
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
)

// TestFileStatusInsideOnDemandRoots pins the division of labour for status
// icons in virtual-files mode (revised 2026-08-18 after v0.1.0.230 made the
// corner badges draw inside cloud roots for the first time).
//
// Inside an on-demand mount, per-item AVAILABILITY — online-only cloud,
// downloaded tick, pinned solid — is the Cloud Files platform's own glyph
// vocabulary, computed from the placeholder state. Our overlay badge saying
// "ok" on top of every item would stamp a synced tick over online-only files
// and erase exactly the distinction virtual files exist to show. So an IDLE
// item inside a mount answers "none" (defer to the native glyph), and the
// overlay only speaks when it knows something the platform doesn't: an active
// transfer ("sync") or a blocked/conflicted item ("warn").
//
// History: before SetOverlayRoots existed, everything in a mount answered
// "none" because FileStatus keyed off the (deliberately empty) pairs — but for
// the WRONG reason: the engine's in-flight/blocked answers were lost too.
// Then it answered "ok" for everything, which was invisible while the phantom
// cloudFiles handler declarations suppressed badge drawing in cloud roots, and
// wrong the moment v0.1.0.230 removed them.
func TestFileStatusInsideOnDemandRoots(t *testing.T) {
	tmp := t.TempDir()
	e := &Engine{dirs: config.Dirs{Config: tmp, Data: tmp}.WithAccount("a")}

	mount := filepath.Join(tmp, "Nextcloud")
	inside := filepath.Join(mount, "Docs", "Manual.pdf")
	outside := filepath.Join(tmp, "Elsewhere", "Other.pdf")

	// No pairs and no overlay roots: nothing is ours.
	if got := e.FileStatus(inside); got != "none" {
		t.Fatalf("with no roots at all, FileStatus = %q, want %q", got, "none")
	}

	e.SetOverlayRoots([]string{mount})

	// Idle items defer to the native cloud glyphs.
	if got := e.FileStatus(inside); got != "none" {
		t.Errorf("idle file inside an on-demand mount: FileStatus = %q, want %q (native glyphs own availability)", got, "none")
	}
	if got := e.FileStatus(mount); got != "none" {
		t.Errorf("the idle mount root itself: FileStatus = %q, want %q", got, "none")
	}

	// An active transfer is information the platform lacks — badge it.
	e.markInflight(inside, true)
	if got := e.FileStatus(inside); got != "sync" {
		t.Errorf("in-flight file inside a mount: FileStatus = %q, want %q", got, "sync")
	}
	e.markInflight(inside, false)
	if got := e.FileStatus(inside); got != "none" {
		t.Errorf("after the transfer, FileStatus = %q, want %q", got, "none")
	}

	// A sibling directory whose name shares the mount's prefix must NOT match:
	// a plain strings.HasPrefix would wrongly claim "…\NextcloudBackup".
	if got := e.FileStatus(mount + "Backup"); got != "none" {
		t.Errorf("sibling with a shared prefix: FileStatus = %q, want %q", got, "none")
	}
	if got := e.FileStatus(outside); got != "none" {
		t.Errorf("file outside every root: FileStatus = %q, want %q", got, "none")
	}

	e.SetOverlayRoots(nil)
	e.markInflight(inside, true)
	if got := e.FileStatus(inside); got != "none" {
		t.Errorf("after clearing the roots even an in-flight path is not ours: FileStatus = %q, want %q", got, "none")
	}
}

// TestFileStatusLivePairsStillAnswerOk pins that the revision above changes
// on-demand mounts ONLY: files in a live sync pair keep the full vocabulary,
// including "ok" — there are no native cloud glyphs in a plain folder, so the
// badge is the only status a live file gets.
func TestFileStatusLivePairsStillAnswerOk(t *testing.T) {
	tmp := t.TempDir()
	d := config.Dirs{Config: tmp, Data: tmp}.WithAccount("a")
	pairDir := filepath.Join(tmp, "Live")
	if err := d.SavePairs([]config.SyncPair{{LocalDir: pairDir, RemoteRoot: ""}}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{dirs: d}

	inside := filepath.Join(pairDir, "doc.txt")
	if got := e.FileStatus(inside); got != "ok" {
		t.Errorf("idle file in a live pair: FileStatus = %q, want %q", got, "ok")
	}
	e.markInflight(inside, true)
	if got := e.FileStatus(inside); got != "sync" {
		t.Errorf("in-flight file in a live pair: FileStatus = %q, want %q", got, "sync")
	}
}

// TestFileStatusIgnoredFilesAnswerNone closes #575's badge half: FileStatus
// answered "ok" for ANYTHING inside a pair, so engine-ignored files (the
// official client's journals, .sync-exclude.lst, user patterns) wore a synced
// tick badge the engine will never honour. They answer "none" now — no badge.
func TestFileStatusIgnoredFilesAnswerNone(t *testing.T) {
	tmp := t.TempDir()
	d := config.Dirs{Config: tmp, Data: tmp}.WithAccount("a")
	pairDir := filepath.Join(tmp, "Live")
	if err := d.SavePairs([]config.SyncPair{{LocalDir: pairDir, RemoteRoot: ""}}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{dirs: d}

	if got := e.FileStatus(filepath.Join(pairDir, ".sync_ab12.db")); got != "none" {
		t.Errorf("built-in-ignored journal: FileStatus = %q, want %q", got, "none")
	}
	if got := e.FileStatus(filepath.Join(pairDir, "sub", ".nextcloudsync.log")); got != "none" {
		t.Errorf("nested ignored journal: FileStatus = %q, want %q", got, "none")
	}
	if got := e.FileStatus(filepath.Join(pairDir, "doc.txt")); got != "ok" {
		t.Errorf("ordinary file: FileStatus = %q, want %q", got, "ok")
	}
}

// TestFileStatusSharedItems pins the shared-folder indicator: an item whose
// remote path carries a share (either direction) answers "shared" so Explorer
// can mark it — but only the shared NODE itself, not everything inside it
// (OneDrive's model), and never while a transfer or problem has something
// more urgent to say. Inside on-demand mounts "shared" wins over the idle
// "none": availability glyphs are native there, sharing is not.
func TestFileStatusSharedItems(t *testing.T) {
	tmp := t.TempDir()
	d := config.Dirs{Config: tmp, Data: tmp}.WithAccount("a")
	pairDir := filepath.Join(tmp, "Live")
	if err := d.SavePairs([]config.SyncPair{{LocalDir: pairDir, RemoteRoot: ""}}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{dirs: d}
	e.SetSharedRemotePaths([]string{"Projects", "Docs/Report.docx"})

	shared := filepath.Join(pairDir, "Projects")
	if got := e.FileStatus(shared); got != "shared" {
		t.Errorf("shared folder: FileStatus = %q, want %q", got, "shared")
	}
	if got := e.FileStatus(filepath.Join(shared, "inner.txt")); got != "ok" {
		t.Errorf("file INSIDE a shared folder: FileStatus = %q, want %q (mark the share root only)", got, "ok")
	}
	if got := e.FileStatus(filepath.Join(pairDir, "Docs", "Report.docx")); got != "shared" {
		t.Errorf("nested shared file: FileStatus = %q, want %q", got, "shared")
	}
	if got := e.FileStatus(filepath.Join(pairDir, "doc.txt")); got != "ok" {
		t.Errorf("unshared file: FileStatus = %q, want %q", got, "ok")
	}

	// Transfers and problems outrank the shared marker.
	e.markInflight(shared, true)
	if got := e.FileStatus(shared); got != "sync" {
		t.Errorf("in-flight shared folder: FileStatus = %q, want %q", got, "sync")
	}
	e.markInflight(shared, false)

	// On-demand mount: shared beats the idle "none", everything else stays native.
	e2 := &Engine{dirs: config.Dirs{Config: tmp, Data: tmp}.WithAccount("b")}
	mount := filepath.Join(tmp, "NC")
	e2.SetOverlayRoots([]string{mount})
	e2.SetSharedRemotePaths([]string{"Team space"})
	if got := e2.FileStatus(filepath.Join(mount, "Team space")); got != "shared" {
		t.Errorf("shared folder in a mount: FileStatus = %q, want %q", got, "shared")
	}
	if got := e2.FileStatus(filepath.Join(mount, "Private")); got != "none" {
		t.Errorf("unshared folder in a mount: FileStatus = %q, want %q", got, "none")
	}
}

// TestAddSyncPairIdempotentOnExactMatch: signing back into an account whose
// pair config survived (only the keychain secret was lost) replays the setup
// flow, which re-adds the same folder. An EXACT duplicate — same local dir,
// same remote root — must be a quiet success, not "that remote folder is
// already synced" (dead-ended the VM's re-login, 2026-08-21). Genuine
// conflicts (same remote to a different folder) stay refused.
func TestAddSyncPairIdempotentOnExactMatch(t *testing.T) {
	tmp := t.TempDir()
	d := config.Dirs{Config: tmp, Data: tmp}.WithAccount("a")
	e := &Engine{dirs: d}
	t.Cleanup(e.closeStore) // AddSyncPair opens the state DB (clone-state clear); release it before TempDir cleanup
	dir := filepath.Join(tmp, "Cloud")

	if err := e.AddSyncPair(dir, ""); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := e.AddSyncPair(dir, ""); err != nil {
		t.Errorf("identical re-add must succeed, got: %v", err)
	}
	pairs, _ := d.LoadPairs()
	if len(pairs) != 1 {
		t.Errorf("re-add duplicated the pair: %d entries", len(pairs))
	}
	if err := e.AddSyncPair(filepath.Join(tmp, "Other"), ""); err == nil {
		t.Error("same remote to a DIFFERENT folder must still be refused")
	}
}
