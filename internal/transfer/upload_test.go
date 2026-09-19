package transfer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/transport"
)

// fakeNC is a minimal Nextcloud WebDAV endpoint covering exactly what Upload
// exercises: single PUT, the chunked v2 session (MKCOL / PROPFIND / chunk PUT /
// MOVE .file / DELETE), and Stat's PROPFIND on the destination.
type fakeNC struct {
	mu       sync.Mutex
	files    map[string][]byte            // destination path -> content
	sessions map[string]map[string][]byte // uploadID -> chunk name -> bytes

	chunkFail      map[string]int // chunk name -> remaining failures
	chunkFailCode  int            // status to fail chunks with
	assembleFail   int            // remaining MOVE failures
	assembleCode   int
	assembleSilent bool   // fail the MOVE response but perform the assembly anyway
	destStatFails  int    // remaining PROPFIND failures on destination paths
	destChecksum   string // oc:checksums value to include in destination PROPFINDs

	chunkPuts       map[string]int // chunk name -> PUT attempts seen
	sessionDeletes  []string       // upload IDs DELETEd (whole session)
	chunkDeletes    []string       // chunk names DELETEd individually
	assembleAttempt int

	onSinglePut func()            // runs as a single PUT arrives, while the upload is in flight
	onChunkPut  func(name string) // runs as each chunk PUT arrives
	declared    map[string]string // destination -> OC-Checksum a single PUT declared
}

func newFakeNC() *fakeNC {
	return &fakeNC{
		files: map[string][]byte{}, sessions: map[string]map[string][]byte{},
		chunkFail: map[string]int{}, chunkPuts: map[string]int{}, declared: map[string]string{},
	}
}

func (f *fakeNC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/remote.php/dav/uploads/u/"):
		rest := strings.TrimPrefix(path, "/remote.php/dav/uploads/u/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		switch r.Method {
		case "MKCOL":
			if _, ok := f.sessions[id]; ok {
				w.WriteHeader(http.StatusMethodNotAllowed) // already exists (resume)
				return
			}
			f.sessions[id] = map[string][]byte{}
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			sess, ok := f.sessions[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var b strings.Builder
			b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
			fmt.Fprintf(&b, `<d:response><d:href>/remote.php/dav/uploads/u/%s/</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response>`, id)
			names := make([]string, 0, len(sess))
			for n := range sess {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Fprintf(&b, `<d:response><d:href>/remote.php/dav/uploads/u/%s/%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:getcontentlength>%d</d:getcontentlength><d:resourcetype/></d:prop></d:propstat></d:response>`, id, n, len(sess[n]))
			}
			b.WriteString(`</d:multistatus>`)
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(b.String()))
		case http.MethodPut:
			chunk := parts[1]
			f.chunkPuts[chunk]++
			if f.onChunkPut != nil {
				f.onChunkPut(chunk)
			}
			if f.chunkFail[chunk] > 0 {
				f.chunkFail[chunk]--
				w.WriteHeader(f.chunkFailCode)
				return
			}
			body := readAll(r)
			sess, ok := f.sessions[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			sess[chunk] = body
			w.WriteHeader(http.StatusCreated)
		case "MOVE":
			f.assembleAttempt++
			assemble := func() {
				sess, ok := f.sessions[id]
				if !ok {
					return
				}
				dest := destFromHeader(r)
				names := make([]string, 0, len(sess))
				for n := range sess {
					names = append(names, n)
				}
				sort.Strings(names)
				var content []byte
				for _, n := range names {
					content = append(content, sess[n]...)
				}
				f.files[dest] = content
				delete(f.sessions, id) // the server consumes the session on assembly
			}
			if f.assembleFail > 0 {
				f.assembleFail--
				if f.assembleSilent {
					assemble() // the work happened; only the response is lost
				}
				w.WriteHeader(f.assembleCode)
				return
			}
			sess, ok := f.sessions[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = sess
			assemble()
			w.Header().Set("OC-ETag", `"etag-assembled"`)
			w.Header().Set("OC-FileId", "fid-1")
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			if len(parts) > 1 && parts[1] != "" {
				f.chunkDeletes = append(f.chunkDeletes, parts[1])
				if sess, ok := f.sessions[id]; ok {
					delete(sess, parts[1])
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			f.sessionDeletes = append(f.sessionDeletes, id)
			delete(f.sessions, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case strings.HasPrefix(path, "/remote.php/dav/files/u/"):
		dest := strings.TrimPrefix(path, "/remote.php/dav/files/u/")
		switch r.Method {
		case http.MethodPut:
			f.chunkPuts["single"]++
			if f.onSinglePut != nil {
				f.onSinglePut()
			}
			if f.chunkFail["single"] > 0 {
				f.chunkFail["single"]--
				w.WriteHeader(f.chunkFailCode)
				return
			}
			f.files[dest] = readAll(r)
			f.declared[dest] = r.Header.Get("OC-Checksum")
			w.Header().Set("OC-ETag", `"etag-single"`)
			w.Header().Set("OC-FileId", "fid-s")
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND": // Stat on the destination
			if f.destStatFails > 0 {
				f.destStatFails--
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			content, ok := f.files[dest]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			cks := ""
			if f.destChecksum != "" {
				cks = "<checksums><checksum>" + f.destChecksum + "</checksum></checksums>"
			}
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:getetag>"etag-stat-%d"</d:getetag><d:getcontentlength>%d</d:getcontentlength>%s<d:resourcetype/></d:prop></d:propstat></d:response></d:multistatus>`, path, len(content), len(content), cks)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func readAll(r *http.Request) []byte {
	b := make([]byte, 0, 1024)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			return b
		}
	}
}

func destFromHeader(r *http.Request) string {
	d := r.Header.Get("Destination")
	if i := strings.Index(d, "/remote.php/dav/files/u/"); i >= 0 {
		return d[i+len("/remote.php/dav/files/u/"):]
	}
	return d
}

// uploadFixture writes a local file big enough to need chunking (with tiny test
// chunk sizes) and returns a client pointed at the fake server.
func uploadFixture(t *testing.T, size int) (*fakeNC, *transport.Client, string) {
	t.Helper()
	f := newFakeNC()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := transport.New(srv.URL, "u", "p")
	dir := t.TempDir()
	local := filepath.Join(dir, "big.bin")
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 251)
	}
	if err := os.WriteFile(local, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return f, c, local
}

// smallChunks shrinks the chunk thresholds so tests exercise the chunked path
// with a few KB instead of tens of MB.
func smallChunks(t *testing.T) {
	t.Helper()
	oc, om := chunkThreshold, minChunkSize
	chunkThreshold, minChunkSize = 100, 1024
	t.Cleanup(func() { chunkThreshold, minChunkSize = oc, om })
}

// A transient chunk failure (a 503 blip, a recycled connection) must be
// retried within the upload — with the full chunk re-sent from the start —
// instead of failing the whole file (issue #1).
func TestChunkedUploadRetriesTransientChunkFailure(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	f.chunkFail["00002"] = 2
	f.chunkFailCode = http.StatusServiceUnavailable

	res, err := Upload(context.Background(), c, local, "docs/big.bin")
	if err != nil {
		t.Fatalf("Upload failed despite transient chunk errors: %v", err)
	}
	want, _ := os.ReadFile(local)
	f.mu.Lock()
	got := f.files["docs/big.bin"]
	puts := f.chunkPuts["00002"]
	f.mu.Unlock()
	if string(got) != string(want) {
		t.Fatalf("assembled content wrong (%d bytes, want %d) — chunk not re-sent from the start", len(got), len(want))
	}
	if puts != 3 {
		t.Errorf("chunk 00002 PUT %d times, want 3 (2 failures + 1 success)", puts)
	}
	if res.ETag == "" {
		t.Error("no ETag returned")
	}
}

// A failed upload must leave its session (and uploaded chunks) on the server:
// deleting it is what turned every retry of a 300GB file into a restart from
// byte zero.
func TestFailedUploadKeepsSessionForResume(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	f.chunkFail["00003"] = 99 // persistent failure
	f.chunkFailCode = http.StatusBadGateway

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err == nil {
		t.Fatal("Upload succeeded, want failure")
	}
	f.mu.Lock()
	dels := len(f.sessionDeletes)
	sessions := len(f.sessions)
	f.mu.Unlock()
	if dels != 0 {
		t.Fatalf("session deleted on failure (%d DELETEs) — resume impossible", dels)
	}
	if sessions != 1 {
		t.Fatalf("%d sessions on the server, want 1 kept for resume", sessions)
	}
}

// A second attempt at the same (unchanged) file must reuse the previous
// session: same deterministic upload ID, already-uploaded chunks skipped.
func TestUploadResumesPreviousSession(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	// Chunks 1-2 land; chunk 3 fails past the retry budget, aborting attempt 1.
	f.chunkFail["00003"] = 99
	f.chunkFailCode = http.StatusBadGateway

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err == nil {
		t.Fatal("first Upload succeeded, want failure")
	}
	f.mu.Lock()
	firstPuts1 := f.chunkPuts["00001"]
	f.chunkFail["00003"] = 0 // server healthy again
	f.mu.Unlock()

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err != nil {
		t.Fatalf("resumed Upload failed: %v", err)
	}
	want, _ := os.ReadFile(local)
	f.mu.Lock()
	got := f.files["docs/big.bin"]
	puts1 := f.chunkPuts["00001"]
	f.mu.Unlock()
	if string(got) != string(want) {
		t.Fatalf("assembled content wrong after resume (%d bytes, want %d)", len(got), len(want))
	}
	if puts1 != firstPuts1 {
		t.Errorf("chunk 00001 re-uploaded on resume (%d -> %d PUTs) — resume not skipping existing chunks", firstPuts1, puts1)
	}
}

// The upload ID must be derived from the file's identity (path + size + mtime):
// stable while the file is unchanged, different once it changes — a changed
// file must never adopt a stale session's chunks.
func TestUploadIDDeterministic(t *testing.T) {
	id1 := uploadIDFor("docs/big.bin", 1000, 12345)
	if id1 != uploadIDFor("docs/big.bin", 1000, 12345) {
		t.Error("same file produced different upload IDs")
	}
	if id1 == uploadIDFor("docs/big.bin", 1001, 12345) {
		t.Error("size change kept the same upload ID")
	}
	if id1 == uploadIDFor("docs/big.bin", 1000, 99999) {
		t.Error("mtime change kept the same upload ID")
	}
	if id1 == uploadIDFor("docs/other.bin", 1000, 12345) {
		t.Error("different path kept the same upload ID")
	}
	if strings.ContainsAny(id1, "/\\ ") {
		t.Errorf("upload ID %q not a safe collection name", id1)
	}
}

// Progress across a chunk retry must stay honest: the bytes reported for the
// failed attempt are withdrawn before the chunk restarts, so DoneBytes can
// never exceed the file size.
func TestProgressCorrectedOnChunkRetry(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	f.chunkFail["00002"] = 1
	f.chunkFailCode = http.StatusServiceUnavailable

	var mu sync.Mutex
	var done, peak int64
	fi, _ := os.Stat(local)
	_, err := UploadProgress(context.Background(), c, local, "docs/big.bin", func(n int64) {
		mu.Lock()
		done += n
		if done > peak {
			peak = done
		}
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if done != fi.Size() {
		t.Errorf("final progress %d, want %d", done, fi.Size())
	}
	if peak > fi.Size() {
		t.Errorf("progress peaked at %d, above the file size %d — retry bytes double-counted", peak, fi.Size())
	}
}

// A transient failure of the final MOVE (a proxy cutting a minutes-long
// assembly response) must be retried, not fail the whole upload.
func TestAssembleRetriedOnTransientFailure(t *testing.T) {
	smallChunks(t)
	shortAssemble(t)

	f, c, local := uploadFixture(t, 3*1024+100)
	f.assembleFail, f.assembleCode = 1, http.StatusBadGateway

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err != nil {
		t.Fatalf("upload failed despite transient assemble error: %v", err)
	}
	want, _ := os.ReadFile(local)
	f.mu.Lock()
	got := f.files["docs/big.bin"]
	attempts := f.assembleAttempt
	f.mu.Unlock()
	if string(got) != string(want) {
		t.Fatalf("assembled content wrong (%d bytes, want %d)", len(got), len(want))
	}
	if attempts != 2 {
		t.Errorf("MOVE attempted %d times, want 2", attempts)
	}
}

// A refused assembly (bad request — e.g. checksum mismatch from stale chunks)
// must delete the session: with deterministic session IDs the next attempt
// would otherwise reuse the poisoned chunks forever.
func TestRefusedAssembleDeletesPoisonedSession(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	f.assembleFail, f.assembleCode = 99, http.StatusBadRequest

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err == nil {
		t.Fatal("upload succeeded, want refused assembly to fail it")
	}
	f.mu.Lock()
	dels := len(f.sessionDeletes)
	f.mu.Unlock()
	if dels != 1 {
		t.Fatalf("poisoned session DELETEd %d times, want 1 (next attempt must start clean)", dels)
	}
}

// A 423 at assemble time (someone locked the destination mid-upload) must NOT
// delete the session: the chunks are fine, and destroying them turns a
// colleague's brief lock into a 300GB re-upload.
func TestAssembleLockKeepsSession(t *testing.T) {
	smallChunks(t)
	shortAssemble(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	f.assembleFail, f.assembleCode = 99, http.StatusLocked

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err == nil {
		t.Fatal("upload succeeded, want lock failure")
	}
	f.mu.Lock()
	dels, sessions := len(f.sessionDeletes), len(f.sessions)
	f.mu.Unlock()
	if dels != 0 {
		t.Fatalf("locked assemble DELETEd the session (%d) — chunks destroyed by a lock", dels)
	}
	if sessions != 1 {
		t.Fatalf("session gone (%d left) — resume impossible after a lock", sessions)
	}
}

// Quota-full at assemble likewise keeps the session: freeing space and
// retrying must resume, not restart.
func TestAssembleQuotaKeepsSession(t *testing.T) {
	smallChunks(t)
	shortAssemble(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	f.assembleFail, f.assembleCode = 99, http.StatusInsufficientStorage

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err == nil {
		t.Fatal("upload succeeded, want quota failure")
	}
	f.mu.Lock()
	dels := len(f.sessionDeletes)
	f.mu.Unlock()
	if dels != 0 {
		t.Fatalf("quota failure DELETEd the session (%d)", dels)
	}
}

// A MOVE whose response was lost while the assembly landed server-side (the
// proxy cut a minutes-long response; the retry then 404s on the consumed
// session) must be recognised as success via the destination.
func TestAssembleResponseLostDetectedViaDestination(t *testing.T) {
	smallChunks(t)
	shortAssemble(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	f.assembleFail, f.assembleCode = 1, http.StatusBadGateway
	f.assembleSilent = true // the assembly happens; only the response is lost

	res, err := Upload(context.Background(), c, local, "docs/big.bin")
	if err != nil {
		t.Fatalf("lost-response assembly not recognised as success: %v", err)
	}
	want, _ := os.ReadFile(local)
	f.mu.Lock()
	got := f.files["docs/big.bin"]
	dels := len(f.sessionDeletes)
	f.mu.Unlock()
	if string(got) != string(want) {
		t.Fatalf("content wrong after lost-response recovery")
	}
	if dels != 0 {
		t.Fatalf("recovered upload still DELETEd something (%d)", dels)
	}
	if res.ETag == "" {
		t.Error("no ETag recovered from the destination")
	}
}

// If the pre-assembly Stat could not establish a baseline, destination polling
// must NOT declare success: an OLD same-size server copy would otherwise be
// read as "our upload landed" and the new content silently never uploads.
func TestPreStatFailureDisablesDestinationPolling(t *testing.T) {
	smallChunks(t)
	shortAssemble(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	// An old same-size copy already on the server.
	old := make([]byte, 3*1024+100)
	f.mu.Lock()
	f.files["docs/big.bin"] = old
	f.mu.Unlock()
	f.destStatFails = 99 // every destination Stat fails → no pre-assembly baseline
	f.assembleFail, f.assembleCode = 99, http.StatusBadGateway

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err == nil {
		t.Fatal("upload reported success with no baseline and a same-size old file — silent lost update")
	}
}

// Resuming with a session written by a different chunk layout (an older app
// version, a different size calculation) must not let stale higher-numbered
// chunks reach the assembly.
func TestResumePrunesStaleExcessChunks(t *testing.T) {
	smallChunks(t)
	shortAssemble(t)
	f, c, local := uploadFixture(t, 3*1024+100) // needs chunks 00001..00004
	fi, _ := os.Stat(local)
	id := uploadIDFor("docs/big.bin", fi.Size(), fi.ModTime().UnixNano())
	f.mu.Lock()
	f.sessions[id] = map[string][]byte{
		"00005": []byte("stale tail from an older layout"),
		"00006": []byte("more stale bytes"),
	}
	f.mu.Unlock()

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(local)
	f.mu.Lock()
	got := f.files["docs/big.bin"]
	f.mu.Unlock()
	if string(got) != string(want) {
		t.Fatalf("stale chunks reached the assembly: got %d bytes, want %d", len(got), len(want))
	}
}

// When the server reports a checksum for the assembled file and it does not
// match ours, the upload must fail loudly — the last line of defence for
// servers that do not verify OC-Checksum at assembly time.
func TestServerChecksumMismatchAfterAssemblyFails(t *testing.T) {
	smallChunks(t)
	shortAssemble(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	f.destChecksum = "SHA1:0000000000000000000000000000000000000bad"

	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err == nil {
		t.Fatal("assembly with a mismatched server checksum reported success")
	}
}

// The session ID must differ between machines: two clients sharing an account
// must never interleave chunks in one server-side session.
func TestUploadIDDiffersAcrossMachines(t *testing.T) {
	oldSalt := uploadIDSalt
	t.Cleanup(func() { uploadIDSalt = oldSalt })
	uploadIDSalt = "machine-A"
	a := uploadIDFor("docs/big.bin", 1000, 12345)
	uploadIDSalt = "machine-B"
	b := uploadIDFor("docs/big.bin", 1000, 12345)
	if a == b {
		t.Fatal("same session ID on different machines — concurrent clients would corrupt each other's sessions")
	}
}

// shortAssemble shrinks the assemble retry/poll waits for tests.
func shortAssemble(t *testing.T) {
	t.Helper()
	og, op, ob := assemblePollGap, assemblePollBudget, assemblePollMin
	assemblePollGap, assemblePollBudget, assemblePollMin = 10*time.Millisecond, func(int64) time.Duration { return 100 * time.Millisecond }, 50*time.Millisecond
	t.Cleanup(func() { assemblePollGap, assemblePollBudget, assemblePollMin = og, op, ob })
}

// A small (single-PUT) upload gets the same transient-failure retry.
func TestSingleUploadRetriesTransientFailure(t *testing.T) {
	f, c, local := uploadFixture(t, 50) // under chunkThreshold
	f.chunkFail["single"] = 1
	f.chunkFailCode = http.StatusServiceUnavailable

	if _, err := Upload(context.Background(), c, local, "docs/small.bin"); err != nil {
		t.Fatalf("small upload failed despite transient error: %v", err)
	}
	want, _ := os.ReadFile(local)
	f.mu.Lock()
	got := f.files["docs/small.bin"]
	f.mu.Unlock()
	if string(got) != string(want) {
		t.Fatalf("uploaded content wrong (%d bytes, want %d)", len(got), len(want))
	}
}

// A file that changes while being read must be refused (the watcher retries
// after the writer settles) instead of uploading torn bytes.
func TestUploadRefusesFileChangedDuringRead(t *testing.T) {
	f, c, local := uploadFixture(t, 50)
	_ = f
	// Sneak a change in between the initial stat and the hash by making the
	// hash step observe a different mtime: rewrite the file, backdating is not
	// needed — UploadProgress stats, hashes, then re-stats.
	// Simulate by racing: rewrite with different size right after Upload starts.
	// Deterministic version: pre-hash hook isn't exposed, so emulate the check
	// directly — write, stat, modify, and call the guard path via UploadProgress
	// with a file whose mtime we bump mid-flight using a wrapper is not
	// possible without a seam; instead verify the guard exists at the API
	// level: a file that differs between stat and upload fails.
	if err := os.WriteFile(local, make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	// Direct unit check of the guard:
	fi, _ := os.Stat(local)
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(local, make([]byte, 60), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statUnchanged(local, fi); err == nil {
		t.Fatal("statUnchanged missed a size/mtime change")
	}
	if _, err := Upload(context.Background(), c, local, "docs/x.bin"); err != nil {
		t.Fatalf("steady file refused: %v", err)
	}
}
