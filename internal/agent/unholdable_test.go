package agent

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transport"
)

func unholdEngine(t *testing.T, server string) *Engine {
	t.Helper()
	e := &Engine{
		dirs:    config.Dirs{Config: t.TempDir(), Data: t.TempDir()}.WithAccount("a"),
		blocked: map[string][]engine.Blocked{},
	}
	if server != "" {
		e.client = transport.New(server, "alice", "pw")
	}
	return e
}

func unholdRows(e *Engine) map[string]BlockedFile {
	out := map[string]BlockedFile{}
	for _, b := range e.BlockedFiles() {
		if b.Remote != "" {
			out[b.Remote] = b
		}
	}
	return out
}

// Deck #984: a server item Windows cannot hold must show up in "Can't sync"
// (not go quietly missing), stay there across a restart, and drop off once a
// listing of its folder no longer has it.
func TestUnholdableItemsAreShownAndKept(t *testing.T) {
	e := unholdEngine(t, "")
	local := `C:\Nimbo`
	e.SetUnholdable(local, "", "Clients/SL", []engine.Unholdable{{Remote: "Clients/SL/Pilot Validation ", IsDir: true}})
	e.SetUnholdable(local, "", "Clients", nil) // another folder's listing must not touch it

	rows := unholdRows(e)
	b, ok := rows["Clients/SL/Pilot Validation "]
	if !ok {
		t.Fatalf("not in Can't sync: %v", rows)
	}
	if b.Reason != UnholdableReason || !b.IsDir || b.Path != "Clients/SL/Pilot Validation " {
		t.Errorf("row = %+v", b)
	}

	// A restart: a fresh engine on the same config reads the saved list.
	e2 := &Engine{dirs: e.dirs, blocked: map[string][]engine.Blocked{}}
	if _, ok := unholdRows(e2)["Clients/SL/Pilot Validation "]; !ok {
		t.Error("the list did not survive a restart")
	}

	// Its folder listed again without it (renamed on the web): it goes.
	e2.SetUnholdable(local, "", "Clients/SL", nil)
	if n := len(unholdRows(e2)); n != 0 {
		t.Errorf("%d rows left after the folder listed clean", n)
	}
}

// A pair rooted below the files root shows a pair-relative path.
func TestUnholdableRowIsPairRelative(t *testing.T) {
	e := unholdEngine(t, "")
	e.SetUnholdable(`C:\Work`, "Team", "Team/a", []engine.Unholdable{{Remote: "Team/a/report."}})
	b := unholdRows(e)["Team/a/report."]
	if b.Path != "a/report." || b.Abs != filepath.Join(`C:\Work`, "a", "report.") {
		t.Errorf("row = %+v", b)
	}
}

type moveRec struct {
	mu        sync.Mutex
	src, dst  string
	overwrite string
	status    int
}

func (m *moveRec) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "MKCOL":
		w.WriteHeader(http.StatusMethodNotAllowed) // the parent exists
	case "MOVE":
		m.mu.Lock()
		m.src, _ = url.PathUnescape(r.URL.EscapedPath())
		d, _ := url.Parse(r.Header.Get("Destination"))
		m.dst, _ = url.PathUnescape(d.EscapedPath())
		m.overwrite = r.Header.Get("Overwrite")
		st := m.status
		m.mu.Unlock()
		w.WriteHeader(st)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestRenameOnServerMovesWithoutReplacing(t *testing.T) {
	m := &moveRec{status: http.StatusCreated}
	srv := httptest.NewServer(m)
	defer srv.Close()
	e := unholdEngine(t, srv.URL)
	e.SetUnholdable(`C:\Nimbo`, "", "SL", []engine.Unholdable{{Remote: "SL/Pilot Validation ", IsDir: true}})

	if err := e.RenameOnServer("SL/Pilot Validation ", "Pilot Validation 2"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(m.src, "/SL/Pilot Validation ") || !strings.HasSuffix(m.dst, "/SL/Pilot Validation 2") {
		t.Errorf("MOVE %q -> %q", m.src, m.dst)
	}
	if m.overwrite != "F" {
		t.Errorf("Overwrite = %q, want F: a rename must never replace what is there", m.overwrite)
	}
	if n := len(unholdRows(e)); n != 0 {
		t.Errorf("still listed after the rename (%d rows)", n)
	}
}

func TestRenameOnServerRefusesATakenOrBadName(t *testing.T) {
	m := &moveRec{status: http.StatusPreconditionFailed}
	srv := httptest.NewServer(m)
	defer srv.Close()
	e := unholdEngine(t, srv.URL)
	e.SetUnholdable(`C:\Nimbo`, "", "SL", []engine.Unholdable{{Remote: "SL/Pilot Validation ", IsDir: true}})

	err := e.RenameOnServer("SL/Pilot Validation ", "Pilot Validation")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("taken name: err = %v", err)
	}
	for _, bad := range []string{"", "x.", "y ", "a/b"} {
		if err := e.RenameOnServer("SL/Pilot Validation ", bad); err == nil {
			t.Errorf("accepted the name %q", bad)
		}
	}
	if _, ok := unholdRows(e)["SL/Pilot Validation "]; !ok {
		t.Error("dropped from Can't sync although the rename failed")
	}
}
