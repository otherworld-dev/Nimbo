package transfer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

// The executor never escapes a directory: escaping renames a basename only,
// so an escaped folder would be created as X.nimboesc while everything inside
// it still uploaded under X/..., into a folder the server does not have.
// engine.FilterBlocked blocks a forbidden folder name before a plan reaches
// here, so any folder that does arrive is created under its own name (Deck
// #554, item 3).
func TestExecutorNeverEscapesADirectory(t *testing.T) {
	var mu sync.Mutex
	var mkcols []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "MKCOL":
			mu.Lock()
			mkcols = append(mkcols, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	esc := engine.NewEscaper(engine.NewForbidden(nil, nil, nil, nil, nil), []string{".htaccess"}, "")
	ex := &Executor{Client: transport.New(srv.URL, "u", "p"), State: st, PairKey: "P", LocalRoot: t.TempDir(), Escaper: esc}

	if _, err := ex.Run(context.Background(), []engine.Action{{Kind: engine.ActCreateRemoteDir, Path: "web/.htaccess"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(mkcols) != 1 || !strings.HasSuffix(mkcols[0], "/web/.htaccess") {
		t.Fatalf("MKCOL paths = %v, want one ending in /web/.htaccess (the folder's own name)", mkcols)
	}
}
