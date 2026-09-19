package transfer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

// Deleting a folder must drop the records of everything in it, not only the
// folder's own. The children's rows otherwise stay behind saying "synced on
// both sides" for files that exist on neither, and the next time the folder
// reappears on one side they read as deletions to send to the other: the
// shape that left 208,726 stale rows under "To Sort" (Deck #691).
func TestFolderDeleteDropsTheRowsBeneathIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, p := range []string{"D", "D/a", "D/sub", "D/sub/b", "D b/c"} {
		if err := st.UpsertBaseline("P", engine.BaselineState{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	ex := &Executor{Client: transport.New(srv.URL, "u", "p"), State: st, PairKey: "P", LocalRoot: t.TempDir()}

	if err := ex.applyDelete(context.Background(), engine.Action{Kind: engine.ActDeleteRemote, Path: "D"}); err != nil {
		t.Fatalf("applyDelete: %v", err)
	}
	got, err := st.LoadBaseline("P")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"D", "D/a", "D/sub", "D/sub/b"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%q outlived the deleted folder", gone)
		}
	}
	if _, ok := got["D b/c"]; !ok {
		t.Error("a sibling's row went with the folder")
	}
}
