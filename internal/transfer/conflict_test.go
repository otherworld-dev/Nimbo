package transfer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

// TestClassifyConflict_DeleteVsEdit covers the asymmetric cases, which don't
// touch the network (no Client/State needed).
func TestClassifyConflict_DeleteVsEdit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{
		LocalRoot: dir,
		Remote: map[string]engine.RemoteState{
			"b.txt": {Path: "b.txt", ETag: "e1"}, // present remotely, absent locally
		},
	}
	ctx := context.Background()

	// a.txt: exists locally, absent remotely → remote was deleted.
	info, merged, err := e.classifyConflict(ctx, engine.Action{Kind: engine.ActConflict, Path: "a.txt"})
	if err != nil || merged {
		t.Fatalf("a.txt: err=%v merged=%v", err, merged)
	}
	if info.Kind != "deleted-remotely" || !info.LocalExists || info.RemoteExists {
		t.Errorf("a.txt: got %+v", info)
	}

	// b.txt: absent locally, present remotely → local was deleted.
	info, merged, err = e.classifyConflict(ctx, engine.Action{Kind: engine.ActConflict, Path: "b.txt"})
	if err != nil || merged {
		t.Fatalf("b.txt: err=%v merged=%v", err, merged)
	}
	if info.Kind != "deleted-locally" || info.LocalExists || !info.RemoteExists {
		t.Errorf("b.txt: got %+v", info)
	}
}

// TestConflictNameDoesNotStackMarkers pins the fix for the field failure where
// a repeatedly-conflicting file grew a fresh " (conflicted copy …)" marker each
// round — six deep on the test VM — until the path crossed MAX_PATH and nothing
// could open it. A conflict of an already-conflicted copy must REPLACE the old
// marker, not append another.
func TestConflictNameDoesNotStackMarkers(t *testing.T) {
	got := ConflictName("New Text Document (conflicted copy 2026-08-16 000722).txt")
	if strings.Count(got, "(conflicted copy") != 1 {
		t.Fatalf("marker stacked: %q", got)
	}
	if !strings.HasPrefix(got, "New Text Document (conflicted copy ") || !strings.HasSuffix(got, ").txt") {
		t.Fatalf("unexpected shape: %q", got)
	}

	// Six stacked markers (the real VM filename) collapse back to one.
	stacked := "New Text Document" + strings.Repeat(" (conflicted copy 2026-08-16 000722)", 6) + ".txt"
	if got := ConflictName(stacked); strings.Count(got, "(conflicted copy") != 1 {
		t.Fatalf("stacked markers survived: %q", got)
	}

	// Directory components and ordinary parentheses are untouched.
	got = ConflictName("a/b (notes) (conflicted copy 2025-01-02 030405).md")
	if !strings.HasPrefix(got, "a/b (notes) (conflicted copy ") || strings.Count(got, "(conflicted copy") != 1 {
		t.Fatalf("got %q", got)
	}
}

// Deciding whether two edited copies differ downloaded the whole server copy to
// compare, on every pass the conflict stood: a 24 GB .pst, every ~14 minutes,
// all evening (Deck #714). The server keeps the SHA1 of what was uploaded, so
// the comparison needs no download when it has one.
func TestConflictCheckComparesServerChecksumWithoutDownloading(t *testing.T) {
	c, gets := conflictServer(t, 0)
	sum, _ := sha1File(writeTemp(t, "local edit"))
	for _, tc := range []struct {
		name       string
		serverSHA1 string
		merged     bool
	}{
		{"same bytes", sum, true},
		{"different bytes", "0123456789abcdef0123456789abcdef01234567", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := conflictExecutor(t, c)
			r := ex.Remote["doc.txt"]
			r.SHA1 = tc.serverSHA1
			ex.Remote["doc.txt"] = r
			before := gets.Load()
			info, merged, err := ex.classifyConflict(context.Background(), engine.Action{Kind: engine.ActConflict, Path: "doc.txt"})
			if err != nil {
				t.Fatal(err)
			}
			if merged != tc.merged {
				t.Errorf("merged = %v, want %v", merged, tc.merged)
			}
			if !tc.merged && info.Kind != "edited" {
				t.Errorf("kind = %q, want edited", info.Kind)
			}
			if n := gets.Load() - before; n != 0 {
				t.Errorf("downloaded the server copy %d times to compare it", n)
			}
		})
	}
}

// Without a server checksum the download is still how the copies are compared.
func TestConflictCheckDownloadsWhenTheServerHasNoChecksum(t *testing.T) {
	c, gets := conflictServer(t, 0)
	ex := conflictExecutor(t, c)
	info, merged, err := ex.classifyConflict(context.Background(), engine.Action{Kind: engine.ActConflict, Path: "doc.txt"})
	if err != nil || merged || info.Kind != "edited" {
		t.Fatalf("info=%+v merged=%v err=%v", info, merged, err)
	}
	if gets.Load() != 1 {
		t.Errorf("GETs = %d, want 1", gets.Load())
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
