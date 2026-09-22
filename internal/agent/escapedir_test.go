package agent

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

// A clone lists the server itself rather than through engine.RemoteScan, and
// decoded every name it saw. Directories are never escaped, so a server
// folder whose name merely looks escaped keeps that name locally; decoding it
// gave the folder one local name and its children another (a child's own
// basename never decodes), so ".htaccess.nimboesc/readme.txt" landed beside
// an empty ".htaccess" folder (Deck #554, item 3).
func TestCloneLeavesAnEscapedLookingDirectoryAlone(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"":                              {isDir: true, etag: "e-root"},
		".htaccess.nimboesc":            {isDir: true, etag: "e-d"},
		".htaccess.nimboesc/readme.txt": {etag: "e-r", body: "hi"},
	})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e, _ := newHookEngine(t, srv.URL)
	fb := engine.NewForbidden(nil, nil, nil, nil, nil)
	e.forbidden.Store(fb)
	e.escaper.Store(engine.NewEscaper(fb, []string{".htaccess"}, ""))
	p := Pair{LocalDir: t.TempDir()}

	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("clone: %v", err)
	}

	if !localExists(t, p, ".htaccess.nimboesc/readme.txt") {
		t.Fatal("the folder's file did not land under the folder's own name")
	}
	if localExists(t, p, ".htaccess") {
		t.Fatal("a folder named .htaccess was invented")
	}
}
