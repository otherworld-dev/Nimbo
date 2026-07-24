package transfer

import (
	"context"
	"os"
	"path/filepath"
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
