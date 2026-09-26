package transfer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

// Keep mine and Keep server used to send the whole file inside the click: a
// 23 GB .pst with no progress, an error only in the log, and every repeat
// click another upload of the same file (Deck #714). The choice is now
// recorded and the next pass does the transfer, with its progress, the wait
// for Outlook and its retries. What matters is what that pass will do, so the
// test asks the real diff.
func TestRecordedChoiceLeavesTheTransferToTheNextPass(t *testing.T) {
	for _, tc := range []struct {
		choice Choice
		want   engine.ActionKind
	}{
		{ChoiceKeepLocal, engine.ActUpload},
		{ChoiceKeepRemote, engine.ActDownload},
	} {
		t.Run(tc.want.String(), func(t *testing.T) {
			ex, transfers := recordChoiceFixture(t, true)
			if err := ex.RecordChoice(context.Background(), "doc.txt", tc.choice); err != nil {
				t.Fatal(err)
			}
			if n := transfers.Load(); n != 0 {
				t.Fatalf("%d transfers made inside the choice", n)
			}
			if got := nextPass(t, ex, true); got != tc.want {
				t.Fatalf("next pass plans %v, want %v", got, tc.want)
			}
		})
	}
}

// When only the chosen side still has the file, the next pass sends it as new.
func TestRecordedChoiceOfTheOnlyCopySendsItAsNew(t *testing.T) {
	ex, transfers := recordChoiceFixture(t, false) // gone from the server
	if err := ex.RecordChoice(context.Background(), "doc.txt", ChoiceKeepLocal); err != nil {
		t.Fatal(err)
	}
	if transfers.Load() != 0 {
		t.Fatal("transferred inside the choice")
	}
	if got := nextPass(t, ex, false); got != engine.ActUpload {
		t.Fatalf("next pass plans %v, want an upload", got)
	}
}

// recordChoiceFixture is an edited doc.txt on both sides: a baseline older than
// either, a local file and (when onServer) a server copy with ETag "e3".
func recordChoiceFixture(t *testing.T, onServer bool) (*Executor, *atomic.Int32) {
	t.Helper()
	var transfers atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			if !onServer {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:getetag>"e3"</d:getetag><oc:fileid>fid</oc:fileid><d:getcontentlength>6</d:getcontentlength><d:resourcetype/></d:prop></d:propstat></d:response></d:multistatus>`, r.URL.Path)
		default:
			transfers.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertBaseline("P", engine.BaselineState{Path: "doc.txt", RemoteETag: "e1", LocalSize: 3, LocalMTimeNanos: 1}); err != nil {
		t.Fatal(err)
	}
	return &Executor{Client: transport.New(srv.URL, "u", "p"), State: st, PairKey: "P", LocalRoot: dir}, &transfers
}

// nextPass diffs doc.txt as the next sync would see it and returns its action.
func nextPass(t *testing.T, ex *Executor, onServer bool) engine.ActionKind {
	t.Helper()
	base, err := ex.State.LoadBaselinePaths("P", []string{"doc.txt"})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(ex.LocalRoot, "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	local := map[string]engine.LocalState{"doc.txt": {Path: "doc.txt", Size: fi.Size(), MTime: fi.ModTime()}}
	remote := map[string]engine.RemoteState{}
	if onServer {
		remote["doc.txt"] = engine.RemoteState{Path: "doc.txt", ETag: "e3", FileID: "fid", Size: 6}
	}
	for _, a := range engine.Diff(base, remote, local) {
		if a.Path == "doc.txt" {
			return a.Kind
		}
	}
	return engine.ActNoop
}
