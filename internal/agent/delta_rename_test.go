package agent

// A folder renamed on the server, picked up by the remote-delta fast path
// (a push or a poll), Deck #947.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The delta used to plan a server-side folder rename as "delete every file,
// download every file": only the full pass coalesced renames. So renaming a
// folder in the web UI re-downloaded all of it on every client, and the
// deletions it was counted as tripped the damage guard (61 of 91 files on the
// test VM). The delta now coalesces too: the local files are renamed, nothing
// is downloaded and nothing is paused.
func TestRemoteDeltaRenamesAFolderInsteadOfRedownloadingIt(t *testing.T) {
	nodes := map[string]davNode{
		"":      {isDir: true, etag: "e-root"},
		"Batch": {isDir: true, etag: "e-batch", fileid: "d1"},
	}
	for i := 1; i <= 30; i++ {
		p := fmt.Sprintf("a%03d.txt", i)
		nodes[p] = davNode{etag: "e-" + p, body: "top " + p, fileid: "a" + fmt.Sprint(i)}
	}
	for i := 1; i <= 60; i++ {
		p := fmt.Sprintf("Batch/b%03d.txt", i)
		nodes[p] = davNode{etag: "e-" + p, body: "batch " + p, fileid: "b" + fmt.Sprint(i)}
	}
	f := newFakeDAV(nodes)
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)
	local := t.TempDir()
	pair := Pair{LocalDir: local, RemoteRoot: ""}
	ctx := context.Background()
	if _, err := e.SyncOnce(ctx, pair); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "Batch", "b060.txt")); err != nil {
		t.Fatalf("first sync did not bring the folder down: %v", err)
	}

	// Rename Batch -> Batch-renamed on the server: same file ids and file
	// etags, new folder etags up the tree (what Nextcloud does on a MOVE).
	for i := 1; i <= 60; i++ {
		old := fmt.Sprintf("Batch/b%03d.txt", i)
		n := nodes[old]
		f.delNode(old)
		f.setNode(fmt.Sprintf("Batch-renamed/b%03d.txt", i), n)
	}
	f.delNode("Batch")
	f.setNode("Batch-renamed", davNode{isDir: true, etag: "e-batch-2", fileid: "d1"})
	f.setNode("", davNode{isDir: true, etag: "e-root-2"})
	before := len(f.gets())

	if _, err := e.syncRemoteDelta(ctx, pair); err != nil {
		t.Fatalf("a folder rename on the server stopped the delta: %v", err)
	}
	if got := len(f.gets()) - before; got != 0 {
		t.Errorf("%d files were downloaded again for a rename, want 0", got)
	}
	for _, i := range []int{1, 30, 60} {
		name := fmt.Sprintf("b%03d.txt", i)
		b, err := os.ReadFile(filepath.Join(local, "Batch-renamed", name))
		if err != nil || string(b) != "batch Batch/"+name {
			t.Errorf("Batch-renamed/%s = %q (err %v), want the file that was already here", name, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(local, "Batch")); !os.IsNotExist(err) {
		t.Errorf("the old folder is still here (err %v)", err)
	}
	if views := e.FrozenViews(); len(views) != 0 {
		t.Errorf("the damage guard paused the folder over a rename: %+v", views)
	}
}
