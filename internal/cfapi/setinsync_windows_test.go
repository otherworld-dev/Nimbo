//go:build windows

package cfapi

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// TestSetInSyncRestoresTheBitAMoveCleared is the live half of fix wave 3: a
// rename or move by another process clears a placeholder's in-sync bit even
// when it carries no local change, and SetInSync is how that bit is put back
// without touching the identity, the data, or the pin state. Explorer draws a
// not-in-sync item — and every folder above it — with "sync pending" arrows
// forever, so a bit nothing will ever look at again is a permanent wrong
// answer in the UI.
//
// The assertions that matter: the bit really was cleared by the move (the
// premise), SetInSync puts it back, the identity is untouched, the file is
// still an online-only stub, and NOTHING was downloaded — SetInSync runs on
// the attributes-only handle (openForCloud), like every other metadata call.
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestSetInSyncRestoresTheBitAMoveCleared(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "insyncroot")
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	var fetches atomic.Int64
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		fetches.Add(1)
		return make([]byte, length), nil
	}
	list := func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} }
	connKey, err := Mount(root, "NimboSetInSyncTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	identity := []byte("remote/move.bin")
	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "move.bin", Size: 4096, ModTime: time.Now(), Identity: identity},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}

	insync := func(path string) bool {
		t.Helper()
		buf, serr := standardInfo(path)
		if serr != nil {
			t.Fatalf("standardInfo(%s): %v", path, serr)
		}
		return (*placeholderStandardInfo)(unsafe.Pointer(&buf[0])).InSyncState != 0
	}

	src := filepath.Join(root, "move.bin")
	if !insync(src) {
		t.Fatal("a freshly created placeholder is not in sync")
	}
	dst := filepath.Join(root, "dst", "move.bin")
	// Another process, or the filter treats it as our own rename and says
	// nothing (see TestRenameCompletionReportsMoves).
	if out, merr := exec.Command("cmd.exe", "/c", "move", "/Y", src, dst).CombinedOutput(); merr != nil {
		t.Fatalf("move: %v: %s", merr, out)
	}
	time.Sleep(300 * time.Millisecond)

	if insync(dst) {
		t.Fatal("the move did NOT clear the in-sync bit — the premise of the whole fix is gone")
	}
	ch, ierr := Inspect(dst)
	if ierr != nil {
		t.Fatalf("Inspect: %v", ierr)
	}
	if ch.NeedsUpload {
		t.Error("a moved online-only stub reads as needing upload — it holds no local data")
	}
	if !ch.Placeholder || ch.InSync {
		t.Errorf("Change{Placeholder:%v, InSync:%v}, want a placeholder that is not in sync", ch.Placeholder, ch.InSync)
	}

	if err := SetInSync(dst); err != nil {
		t.Fatalf("SetInSync: %v", err)
	}
	if !insync(dst) {
		t.Error("SetInSync did not restore the in-sync bit")
	}
	if got, gerr := PlaceholderIdentity(dst); gerr != nil {
		t.Errorf("PlaceholderIdentity after SetInSync: %v", gerr)
	} else if string(got) != string(identity) {
		t.Errorf("identity = %q after SetInSync, want %q (it must not be rewritten)", got, identity)
	}
	if attrs, _, aerr := findAttrTag(dst); aerr != nil {
		t.Fatalf("findAttrTag: %v", aerr)
	} else if attrs&fileAttrRecallOnDataAccess == 0 {
		t.Error("the file is no longer online-only — SetInSync hydrated it")
	}
	if n := fetches.Load(); n != 0 {
		t.Errorf("fetches = %d, want 0 — SetInSync must not pull the file down", n)
	}
	if ch, ierr := Inspect(dst); ierr != nil {
		t.Fatalf("Inspect after SetInSync: %v", ierr)
	} else if !ch.InSync || ch.NeedsUpload {
		t.Errorf("after SetInSync: Change{InSync:%v, NeedsUpload:%v}, want in sync and clean", ch.InSync, ch.NeedsUpload)
	}

	// A plain file has no bit to set, and says so rather than failing oddly.
	plain := filepath.Join(root, "plain.bin")
	if werr := os.WriteFile(plain, []byte("hello"), 0o644); werr != nil {
		t.Fatalf("write plain.bin: %v", werr)
	}
	if err := SetInSync(plain); err != ErrNotPlaceholder {
		t.Errorf("SetInSync(plain) = %v, want %v", err, ErrNotPlaceholder)
	}
}

// TestTruncationToZeroCountsAsUnsyncedContent pins the one blind spot the
// wave-3 measurement found: truncating a hydrated placeholder to exactly zero
// leaves CF_PLACEHOLDER_STANDARD_INFO.ModifiedDataSize at 0 (re-read at 200ms,
// 1s and 3s, and after a move — it never moves off 0), so ModifiedDataSize
// alone would call a truncated file clean, the in-sync heal would then declare
// it synced, and the user's truncation would be silently discarded and undone
// by the next dehydrate. An empty file whose data is fully LOCAL (no
// RECALL_ON_DATA_ACCESS) and whose in-sync bit is clear therefore counts as
// unsynced content. An online-only stub keeps that attribute, so the case this
// predicate exists for is unaffected — that is asserted here too, side by side,
// because the two states differ by nothing else.
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestTruncationToZeroCountsAsUnsyncedContent(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "truncroot")
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })
	connKey, err := Mount(root, "NimboTruncTest", "",
		func(identity []byte, offset, length int64) ([]byte, error) { return make([]byte, length), nil },
		func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} })
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)
	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "trunc.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/trunc.bin")},
		{Name: "empty.bin", Size: 0, ModTime: time.Now(), Identity: []byte("remote/empty.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}

	// A hydrated file truncated to nothing: unsynced content, however the
	// placeholder info reports its size.
	tr := filepath.Join(root, "trunc.bin")
	if _, rerr := os.ReadFile(tr); rerr != nil { // hydrate it
		t.Fatalf("hydrate trunc.bin: %v", rerr)
	}
	time.Sleep(200 * time.Millisecond)
	if mod, merr := PlaceholderModified(tr); merr != nil {
		t.Fatalf("PlaceholderModified(hydrated clean): %v", merr)
	} else if mod {
		t.Error("a hydrated, untouched file reads as holding unsynced content")
	}
	if terr := os.Truncate(tr, 0); terr != nil {
		t.Fatalf("truncate: %v", terr)
	}
	time.Sleep(300 * time.Millisecond)
	if mod, merr := PlaceholderModified(tr); merr != nil {
		t.Fatalf("PlaceholderModified(truncated): %v", merr)
	} else if !mod {
		t.Error("a file truncated to zero reads as clean — the truncation would never be uploaded")
	}
	if ch, ierr := Inspect(tr); ierr != nil {
		t.Fatalf("Inspect(truncated): %v", ierr)
	} else if !ch.NeedsUpload {
		t.Error("Inspect says a truncated-to-zero file needs no upload")
	}

	// An EMPTY online-only stub that another process moved is the state that
	// looks identical bar the attribute — and it must stay clean.
	empty := filepath.Join(root, "empty.bin")
	moved := filepath.Join(root, "dst", "empty.bin")
	if out, merr := exec.Command("cmd.exe", "/c", "move", "/Y", empty, moved).CombinedOutput(); merr != nil {
		t.Fatalf("move empty.bin: %v: %s", merr, out)
	}
	time.Sleep(300 * time.Millisecond)
	if mod, merr := PlaceholderModified(moved); merr != nil {
		t.Fatalf("PlaceholderModified(moved empty stub): %v", merr)
	} else if mod {
		t.Error("a moved, empty, online-only stub reads as holding unsynced content")
	}
	if ch, ierr := Inspect(moved); ierr != nil {
		t.Fatalf("Inspect(moved empty stub): %v", ierr)
	} else if ch.NeedsUpload || ch.InSync {
		t.Errorf("moved empty stub: Change{NeedsUpload:%v, InSync:%v}, want clean and not in sync", ch.NeedsUpload, ch.InSync)
	}
}

// TestSetInSyncRefusesADirectoryPlaceholder pins the refusal the wave-3 review
// asked for: SetInSync is the heal for a FILE whose bit a rename cleared, and
// a directory's bit is owned elsewhere (set at creation, by its repoint, and
// by SweepDirsInSync). The call is refused and the directory is left exactly
// as it was. The directory is put in the state older versions created every
// folder in (not in sync) first, so an accidental mark would show.
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestSetInSyncRefusesADirectoryPlaceholder(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "dirinsyncroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	list := func(rel string) []PlaceholderInfo {
		t.Logf("FETCH_PLACEHOLDERS for %q", rel)
		switch rel {
		case "sub":
			return []PlaceholderInfo{
				{Name: "child.txt", Size: 4, ModTime: time.Now(), Identity: []byte("remote/sub/child.txt")},
			}
		case "":
			// A root whose first transfer delivers nothing re-issues
			// FETCH_PLACEHOLDERS forever (Deck #569), so seed it the way Mount
			// does in production.
			return []PlaceholderInfo{
				{Name: "seed.txt", Size: 1, ModTime: time.Now(), Identity: []byte("remote/seed.txt")},
			}
		}
		return []PlaceholderInfo{}
	}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	connKey, err := Mount(root, "NimboDirInSyncTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "sub", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/sub")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	sub := filepath.Join(root, "sub")
	clearInSync(t, sub)
	before := placeholderStateOf(t, sub)

	if err := SetInSync(sub); !errors.Is(err, ErrIsDirectory) {
		t.Fatalf("SetInSync(dir) = %v, want %v", err, ErrIsDirectory)
	}
	if after := placeholderStateOf(t, sub); after != before {
		t.Fatalf("the refused call changed the directory (state 0x%x -> 0x%x)", before, after)
	}
	if ch, ierr := Inspect(sub); ierr != nil {
		t.Fatalf("Inspect(sub): %v", ierr)
	} else if !ch.IsDir || !ch.Placeholder || ch.InSync || ch.NeedsUpload {
		t.Errorf("Inspect(sub) = %+v, want a clean directory placeholder that is NOT in sync", ch)
	}
}
