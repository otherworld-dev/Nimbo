//go:build windows

package cfapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// Streaming hydration: one reader per FETCH_DATA request instead of one HTTP
// request per megabyte, stopped when the filter withdraws the request, and —
// the parked defect these tests close — FAILED instead of abandoned, so the
// caller's open returns an error at once rather than hanging on the filter's
// own timeout. Live-driver tests, opt in with NIMBO_CFAPI_LIVE=1.
//
// One test here (the filter's stall timeout) can only be observed by waiting
// out that 60s timeout, so it needs NIMBO_CFAPI_SLOW=1 as well and is not part
// of a routine live run; TestCancelFetchDataCallbackStopsThatDownload is the
// default guard for the same plumbing.

// streamRoot mounts a temp sync root whose byte-slice HydrateFunc must never
// be reached: it counts its calls (checked by the caller) and fails the
// transfer, so a test that accidentally exercises the fallback loop shows up
// as a failed read rather than as a silent pass.
func streamRoot(t *testing.T, name string) (root string, connKey int64, sliceCalls *atomic.Int64) {
	t.Helper()
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root = filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	sliceCalls = &atomic.Int64{}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		sliceCalls.Add(1)
		return nil, errors.New("byte-slice hydrate must not be used when a stream is installed")
	}
	list := func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} }
	connKey, err := Mount(root, "Nimbo"+name, "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { Unmount(root, connKey) })
	return root, connKey, sliceCalls
}

// transferCounter hooks Debug and counts the DATA-carrying
// CfExecute(TRANSFER_DATA) calls, accepted or rejected alike (both log the
// same prefix) — what "nothing was transferred after the cancel" is about.
// The prefix keeps its closing ")" on purpose, so the "TRANSFER_DATA fail"
// completion line is NOT counted: the filter re-requests the unserved
// remainder under a NEW transfer key, and that successor being failed says
// nothing about the cancelled request.
type transferCounter struct{ n atomic.Int64 }

func countTransfers(t *testing.T) *transferCounter {
	t.Helper()
	c := &transferCounter{}
	old := Debug
	Debug = func(format string, args ...any) {
		if strings.HasPrefix(format, "CfExecute(TRANSFER_DATA)") {
			c.n.Add(1)
		}
		t.Logf("dbg: "+format, args...)
	}
	t.Cleanup(func() { Debug = old })
	return c
}

// patternBytes is a deterministic filler whose period (251) shares no factor
// with any transfer chunk or sector size, so a piece delivered at the wrong
// offset cannot accidentally compare equal.
func patternBytes(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestStreamHydrationUsesOneReaderPerRequest is the whole point of the change:
// a 64 MB file used to cost 64 HydrateFunc calls (64 HTTP GETs); it must now
// cost exactly one reader, opened for the range the filter actually asked for.
func TestStreamHydrationUsesOneReaderPerRequest(t *testing.T) {
	root, connKey, sliceCalls := streamRoot(t, "streamone")
	log := logTransfers(t)

	const size = 64 << 20
	content := patternBytes(size)

	type req struct{ offset, length int64 }
	var mu sync.Mutex
	var reqs []req
	SetHydrateStream(connKey, func(ctx context.Context, identity []byte, offset, length int64) (io.ReadCloser, error) {
		mu.Lock()
		reqs = append(reqs, req{offset, length})
		mu.Unlock()
		if offset < 0 || length < 0 || offset+length > size {
			return nil, fmt.Errorf("request out of range: offset=%d length=%d", offset, length)
		}
		return io.NopCloser(bytes.NewReader(content[offset : offset+length])), nil
	})

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "big.bin", Size: size, ModTime: time.Now(), Identity: []byte("remote/big.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "big.bin"))
	if err != nil {
		t.Fatalf("read the placeholder: %v", err)
	}

	mu.Lock()
	seen := append([]req(nil), reqs...)
	mu.Unlock()
	t.Logf("stream requests: %v", seen)

	if !bytes.Equal(got, content) {
		t.Fatalf("hydrated content differs from what the stream served (got %d bytes, want %d)", len(got), len(content))
	}
	if len(seen) != 1 {
		t.Fatalf("stream opened %d times, want exactly 1 (requests: %v)", len(seen), seen)
	}
	if seen[0].offset != 0 || seen[0].length != size {
		t.Errorf("stream asked for offset=%d length=%d, want offset=0 length=%d", seen[0].offset, seen[0].length, int64(size))
	}
	if n := sliceCalls.Load(); n != 0 {
		t.Errorf("the byte-slice HydrateFunc was called %d times — the stream must take over completely", n)
	}
	// A link fast enough to fill a piece must still use WHOLE pieces: the
	// slow-link timer (see TestStreamHydrationOnASlowLinkKeepsTheFilterFed)
	// must not fragment a fast transfer into more, smaller ones.
	pieces, _ := log.snapshot()
	if want := size / cfTransferChunk; len(pieces) != want {
		t.Errorf("64 MB arrived in %d transfers, want %d whole %d-byte pieces (pieces: %v)", len(pieces), want, int64(cfTransferChunk), pieces)
	}
	for i, p := range pieces {
		if p[1] != cfTransferChunk {
			t.Errorf("piece %d is %d bytes, want a full %d", i, p[1], int64(cfTransferChunk))
		}
	}
}

// throttledReader hands out per bytes every every, standing in for a slow or
// rate-capped link. It reports the moment it parts with its last byte, which
// is when the test samples how many transfers the filter has already had — a
// complete stream is read exactly length bytes and so never has to return
// io.EOF at all.
type throttledReader struct {
	data     []byte
	pos      int
	per      int
	every    time.Duration
	last     time.Time
	drained  func()
	onceDone sync.Once
}

func (r *throttledReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if wait := r.every - time.Since(r.last); wait > 0 && !r.last.IsZero() {
		time.Sleep(wait)
	}
	r.last = time.Now()
	n := r.per
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	if r.pos >= len(r.data) {
		r.onceDone.Do(r.drained)
	}
	return n, nil
}

func (r *throttledReader) Close() error { return nil }

// transferLog records each executed TRANSFER_DATA's offset and length, and any
// CANCEL_FETCH_DATA, from the Debug hook.
type transferLog struct {
	mu      sync.Mutex
	pieces  [][2]int64 // offset, length
	cancels int
}

func logTransfers(t *testing.T) *transferLog {
	t.Helper()
	l := &transferLog{}
	old := Debug
	Debug = func(format string, args ...any) {
		switch {
		case strings.HasPrefix(format, "CfExecute(TRANSFER_DATA)") && len(args) >= 2:
			off, ok1 := args[0].(int64)
			n, ok2 := args[1].(int)
			if ok1 && ok2 {
				l.mu.Lock()
				l.pieces = append(l.pieces, [2]int64{off, int64(n)})
				l.mu.Unlock()
			}
		case strings.HasPrefix(format, "CANCEL_FETCH_DATA"):
			l.mu.Lock()
			l.cancels++
			l.mu.Unlock()
		}
		t.Logf("dbg: "+format, args...)
	}
	t.Cleanup(func() { Debug = old })
	return l
}

func (l *transferLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pieces)
}

func (l *transferLog) snapshot() ([][2]int64, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][2]int64(nil), l.pieces...), l.cancels
}

// TestStreamHydrationOnASlowLinkKeepsTheFilterFed is the regression test for
// the floor a fixed 4 MiB piece put under hydration: the filter withdraws a
// request that goes ~60s without a TRANSFER_DATA, so a link too slow to fill a
// whole piece in time was cancelled before its first transfer, re-requested,
// and cancelled again until the opener gave up — below ~70 KiB/s (a download
// cap the Settings UI happily accepts) hydration could never complete. Pieces
// are now flushed on a timer as well as when full.
//
// The test drives cfPieceFlushAfter down through testPieceFlushAfter rather
// than actually waiting 15s: what needs proving is the mechanism — transfers
// arriving while the stream is still trickling, every non-final piece still
// sector-aligned despite the ragged read sizes, and the request completing
// byte-exact with no cancel. That the production 15s comfortably beats the
// filter's 60s is arithmetic, and the timeout itself is pinned by the
// SLOW-gated test above.
func TestStreamHydrationOnASlowLinkKeepsTheFilterFed(t *testing.T) {
	root, connKey, sliceCalls := streamRoot(t, "streamslow")
	log := logTransfers(t)
	testPieceFlushAfter = time.Second
	t.Cleanup(func() { testPieceFlushAfter = 0 })

	// 2 MiB at ~500 KB/s ≈ 4s of streaming, flushed about once a second. The
	// 50000-byte read size is deliberately NOT a multiple of 4096, so every
	// flush has a tail to carry into the next piece.
	const size = 2 << 20
	content := patternBytes(size)
	var transfersWhileFlowing atomic.Int64
	var opened atomic.Int64
	SetHydrateStream(connKey, func(ctx context.Context, identity []byte, offset, length int64) (io.ReadCloser, error) {
		opened.Add(1)
		if offset != 0 || length != size {
			return nil, fmt.Errorf("unexpected request: offset=%d length=%d", offset, length)
		}
		return &throttledReader{
			data:  content,
			per:   50000,
			every: 100 * time.Millisecond,
			drained: func() {
				transfersWhileFlowing.Store(int64(log.count()))
			},
		}, nil
	})

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "slowlink.bin", Size: size, ModTime: time.Now(), Identity: []byte("remote/slowlink.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}

	start := time.Now()
	got, err := os.ReadFile(filepath.Join(root, "slowlink.bin"))
	if err != nil {
		t.Fatalf("read the placeholder over a slow stream: %v", err)
	}
	t.Logf("hydrated %d bytes in %v", len(got), time.Since(start).Round(time.Millisecond))

	pieces, cancels := log.snapshot()
	if !bytes.Equal(got, content) {
		t.Fatalf("hydrated content differs from what the stream served (got %d bytes, want %d)", len(got), len(content))
	}
	if cancels != 0 {
		t.Errorf("the filter withdrew the request %d times — a stream that keeps flowing must never be cancelled", cancels)
	}
	if n := opened.Load(); n != 1 {
		t.Errorf("the stream was opened %d times, want 1 (a cancel-and-retry loop is the bug this test is about)", n)
	}
	if n := transfersWhileFlowing.Load(); n < 2 {
		t.Errorf("only %d transfers had been executed by the time the stream handed over its last byte, want >= 2 — pieces are not being flushed as the data trickles in (all pieces: %v)", n, pieces)
	}
	// Every piece but the request's last must land on a sector boundary, and
	// together they must cover the range exactly once.
	next := int64(0)
	for i, p := range pieces {
		off, n := p[0], p[1]
		if off != next {
			t.Errorf("piece %d starts at %d, want %d — the carried tail was lost or duplicated (pieces: %v)", i, off, next, pieces)
		}
		if i < len(pieces)-1 && (off%cfTransferAlign != 0 || n%cfTransferAlign != 0) {
			t.Errorf("piece %d (offset=%d len=%d) is not 4096-aligned; only the piece completing the request may be", i, off, n)
		}
		next = off + n
	}
	if next != size {
		t.Errorf("the transfers cover %d bytes, want %d (pieces: %v)", next, int64(size), pieces)
	}
	if n := sliceCalls.Load(); n != 0 {
		t.Errorf("the byte-slice HydrateFunc was called %d times", n)
	}
}

// failAfter serves n bytes and then fails, standing in for a connection that
// drops mid-download.
type failAfter struct {
	remaining int64
	err       error
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.remaining <= 0 {
		return 0, f.err
	}
	if int64(len(p)) > f.remaining {
		p = p[:f.remaining]
	}
	for i := range p {
		p[i] = 0xAB
	}
	f.remaining -= int64(len(p))
	return len(p), nil
}

func (f *failAfter) Close() error { return nil }

// TestStreamHydrationFailureFailsTheOpenPromptly pins the parked defect: a
// download that dies part-way used to leave the request unanswered, so the
// caller's open sat there until the cloud filter's own timeout. The request
// must be COMPLETED with a failure status instead, and the file must be left
// exactly as it was — a clean placeholder, full logical size, nothing to
// upload.
func TestStreamHydrationFailureFailsTheOpenPromptly(t *testing.T) {
	root, connKey, sliceCalls := streamRoot(t, "streamfail")

	const size = 16 << 20
	SetHydrateStream(connKey, func(ctx context.Context, identity []byte, offset, length int64) (io.ReadCloser, error) {
		return &failAfter{remaining: 1 << 20, err: errors.New("connection reset by peer")}, nil
	})

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "half.bin", Size: size, ModTime: time.Now(), Identity: []byte("remote/half.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	path := filepath.Join(root, "half.bin")

	done := make(chan error, 1)
	go func() {
		_, err := os.ReadFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("reading the file succeeded even though the stream failed part-way")
		}
		t.Logf("open failed as it should: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the read did not fail within 10s — the failed request was abandoned, not completed")
	}

	ch, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !ch.Placeholder || ch.NeedsUpload {
		t.Errorf("after a failed hydration: Placeholder=%v NeedsUpload=%v, want true/false", ch.Placeholder, ch.NeedsUpload)
	}
	if fi, serr := os.Stat(path); serr != nil {
		t.Errorf("Stat: %v", serr)
	} else if fi.Size() != size {
		t.Errorf("size = %d after a failed hydration, want the original %d", fi.Size(), int64(size))
	}
	if n := sliceCalls.Load(); n != 0 {
		t.Errorf("the byte-slice HydrateFunc was called %d times — the stream must take over completely", n)
	}
}

// blockAfter serves one full transfer chunk and then blocks until its context
// is cancelled, so the test can observe the cancel arriving mid-download with
// at least one transfer already executed.
type blockAfter struct {
	ctx      context.Context
	served   int64
	total    int64
	firstOut chan struct{}
	once     sync.Once
}

func (b *blockAfter) Read(p []byte) (int, error) {
	if b.served >= b.total {
		b.once.Do(func() { close(b.firstOut) })
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	if int64(len(p)) > b.total-b.served {
		p = p[:b.total-b.served]
	}
	for i := range p {
		p[i] = 0xCD
	}
	b.served += int64(len(p))
	return len(p), nil
}

func (b *blockAfter) Close() error { return nil }

// TestCancelFetchDataCallbackStopsThatDownload drives the callback the way the
// filter does — two forged structures read through the field offsets — so the
// cancel path is covered without waiting on the filter's own timeout (a whole
// minute; see TestStreamHydrationIsCancelledWithTheOpener). It pins the two
// things our code is responsible for: the transfer key is read from the right
// place in CF_CALLBACK_INFO, and the cancel stops THAT download and no other.
func TestCancelFetchDataCallbackStopsThatDownload(t *testing.T) {
	const connKey = int64(0x7F00BEEF)
	p := &provider{path: `C:\nowhere`}
	providers.Store(connKey, p)
	t.Cleanup(func() { providers.Delete(connKey) })

	mine, cancelMine := context.WithCancel(context.Background())
	other, cancelOther := context.WithCancel(context.Background())
	defer cancelMine()
	defer cancelOther()
	const mineKey, otherKey = int64(11), int64(22)
	p.trackFetch(mineKey, cancelMine)
	p.trackFetch(otherKey, cancelOther)

	// int64-backed so the buffers are 8-aligned like the real structures.
	// CF_CALLBACK_INFO is 152 bytes (RequestKey at 144); the Cancel parameters
	// need 32 (Length at 24).
	info := make([]int64, 19)
	params := make([]int64, 4)
	base := uintptr(unsafe.Pointer(&info[0]))
	pbase := uintptr(unsafe.Pointer(&params[0]))
	*(*int64)(unsafe.Pointer(base + ciConnectionKey)) = connKey
	*(*int64)(unsafe.Pointer(base + ciTransferKey)) = mineKey
	*(*uint32)(unsafe.Pointer(pbase + cpCancelFlags)) = 0x2 // CF_CALLBACK_CANCEL_FLAG_IO_ABORTED
	*(*int64)(unsafe.Pointer(pbase + cpCancelOffset)) = 4 << 20
	*(*int64)(unsafe.Pointer(pbase + cpCancelLength)) = 60 << 20

	if got := cancelFetchDataCallback(base, pbase); got != 0 {
		t.Errorf("cancelFetchDataCallback returned %d, want 0", got)
	}
	select {
	case <-mine.Done():
	case <-time.After(time.Second):
		t.Error("the cancelled request's download was not stopped")
	}
	select {
	case <-other.Done():
		t.Error("an unrelated in-flight download was stopped too")
	default:
	}

	// A cancel naming a connection we know nothing about must be harmless.
	*(*int64)(unsafe.Pointer(base + ciConnectionKey)) = connKey + 1
	if got := cancelFetchDataCallback(base, pbase); got != 0 {
		t.Errorf("cancelFetchDataCallback (unknown conn) returned %d, want 0", got)
	}
}

// TestFetchBookkeepingSurvivesKeyReuseAndTeardown pins the two narrow races in
// the in-flight map. Pure bookkeeping: it touches no Windows API, so it runs
// everywhere and costs nothing.
func TestFetchBookkeepingSurvivesKeyReuseAndTeardown(t *testing.T) {
	p := &provider{path: `C:\nowhere`}
	const key = int64(7)

	// A reused transfer key: the finishing request's untrackFetch must not
	// carry off the successor's entry, or the newcomer becomes uncancellable.
	firstCancelled, secondCancelled := false, false
	first, ok := p.trackFetch(key, func() { firstCancelled = true })
	if !ok {
		t.Fatal("trackFetch refused the first request on a live provider")
	}
	second, ok := p.trackFetch(key, func() { secondCancelled = true })
	if !ok {
		t.Fatal("trackFetch refused a request reusing a live key")
	}
	p.untrackFetch(key, first) // the first download's deferred cleanup
	if !p.cancelFetch(key) {
		t.Fatal("the key was forgotten: the successor's download can no longer be cancelled")
	}
	if !secondCancelled || firstCancelled {
		t.Errorf("cancelled the wrong request (first=%v second=%v)", firstCancelled, secondCancelled)
	}
	p.untrackFetch(key, second)
	if p.cancelFetch(key) {
		t.Error("the finished request was left in the map")
	}

	// Teardown: cancelFetches must stop what is running AND refuse anything
	// later, rather than letting a late FETCH_DATA re-create the map and start
	// a download that outlives the connection.
	running, ok := p.trackFetch(key, func() { firstCancelled = true })
	if !ok {
		t.Fatal("trackFetch refused a request before teardown")
	}
	firstCancelled = false
	p.cancelFetches()
	if !firstCancelled {
		t.Error("cancelFetches left an in-flight download running")
	}
	lateCancelled := false
	if late, ok := p.trackFetch(key, func() { lateCancelled = true }); ok || late != nil {
		t.Errorf("trackFetch after teardown returned (%v, %v), want (nil, false)", late, ok)
	}
	if !lateCancelled {
		t.Error("a request arriving after teardown was neither tracked nor cancelled — it would run uncancellable")
	}
	p.fetchMu.Lock()
	inflight := p.inflight
	p.fetchMu.Unlock()
	if inflight != nil {
		t.Errorf("the in-flight map was re-created after teardown: %v", inflight)
	}
	p.untrackFetch(key, running) // must not panic on the nil map
}

// TestStreamHydrationIsCancelledByTheFilterStallTimeout: a hydration request
// that stops making progress is withdrawn by the filter with
// CANCEL_FETCH_DATA. The download must stop — its context cancelled — and
// nothing may be transferred afterwards (the request is no longer ours to
// serve, and a multi-gigabyte download nobody is waiting for is pure waste).
//
// WHAT CANCELS A REQUEST, measured — and it is NOT the opener dying, which is
// what one would guess (2026-09-16, Windows 10.0.26200):
//
//   - The cancel arrives 60s after the REQUEST started, carrying
//     CF_CALLBACK_CANCEL_FLAG_IO_TIMEOUT (0x1), never IO_ABORTED. Measured at
//     60.01s and 60.13s, the second run with the kill deliberately delayed to
//     30s — the cancel still came at exactly 60s after the request, i.e. 30s
//     after that kill. So the kill does not trigger it and does not hurry it.
//   - It is a STALL timeout, not a request lifetime: a stream that keeps
//     flowing is never cancelled however long it runs (VM, v0.1.0.292: a
//     256 MB hydration streams for 10s untouched).
//   - The reader is killed here all the same, because a request made by the
//     provider's OWN process is not timed out at all: with our reader left
//     alive and the provider stalled identically, no cancel arrived in 120s.
//
// Hence the 60s of wall clock, which nothing on our side can shorten, and
// hence the NIMBO_CFAPI_SLOW gate. Run it when Mount's callback table or the
// cancel plumbing changes; TestCancelFetchDataCallbackStopsThatDownload covers
// our half of the same path in 0.00s on every run.
func TestStreamHydrationIsCancelledByTheFilterStallTimeout(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_SLOW") == "" {
		t.Skip("set NIMBO_CFAPI_SLOW=1 too: this test waits out the filter's 60s stall timeout, which cannot be shortened")
	}
	root, connKey, _ := streamRoot(t, "streamcancel")
	transfers := countTransfers(t)

	const size = 64 << 20
	firstOut := make(chan struct{})
	cancelled := make(chan time.Time, 1)
	var opened atomic.Int64
	SetHydrateStream(connKey, func(ctx context.Context, identity []byte, offset, length int64) (io.ReadCloser, error) {
		if opened.Add(1) > 1 {
			// The filter re-requests the unserved remainder after a cancel
			// when someone is still waiting for it. Refusing the retry keeps
			// "nothing was transferred after the cancel" an assertion about
			// the cancelled request rather than a race with its successor.
			return nil, errors.New("a retry after the cancel is not served by this test")
		}
		go func() {
			<-ctx.Done()
			select {
			case cancelled <- time.Now():
			default:
			}
		}()
		return &blockAfter{ctx: ctx, total: cfTransferChunk, firstOut: firstOut}, nil
	})

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "slow.bin", Size: size, ModTime: time.Now(), Identity: []byte("remote/slow.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	path := filepath.Join(root, "slow.bin")

	// A second process, killed while its read is parked inside the filter, is
	// the only way to produce a real CANCEL_FETCH_DATA: this process cannot
	// abort its own blocked synchronous read. `copy <file> NUL` reads the whole
	// file through a cmd.exe builtin, so killing cmd.exe kills the blocked
	// read itself. It is passed as separate arguments deliberately: the obvious
	// `type "<path>" > NUL` needs the redirection parsed by the shell, and
	// os/exec's quoting of that one long argument leaves cmd.exe answering
	// "The filename, directory name, or volume label syntax is incorrect" —
	// the reader then never opens the file and the test silently tests nothing.
	cmd := exec.Command("cmd.exe", "/c", "copy", "/Y", path, "NUL")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the reader: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	var stalled time.Time
	select {
	case <-firstOut:
		// The reader has served its whole chunk and is now blocked, so this
		// is when the request stopped making progress — and, since that chunk
		// comes out of memory, within a few ms of when the request arrived.
		// The 60s timeout is measured from here.
		stalled = time.Now()
	case <-time.After(20 * time.Second):
		t.Fatalf("the stream never got past its first chunk (streams opened: %d)", opened.Load())
	}
	// Give the loop a moment to execute the transfer for that chunk, so "no
	// transfer after the cancel" is a real assertion.
	time.Sleep(500 * time.Millisecond)
	if transfers.n.Load() == 0 {
		t.Fatal("no TRANSFER_DATA was executed before the cancel — the test never reached the state it is about")
	}

	killedAt := time.Now()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the reader: %v", err)
	}
	<-waited

	select {
	case at := <-cancelled:
		t.Logf("the stream context was cancelled %v after the download stalled (%v after the kill)", at.Sub(stalled), at.Sub(killedAt))
	case <-time.After(90 * time.Second):
		t.Fatal("the stream context was not cancelled within 90s of the download stalling — CANCEL_FETCH_DATA is not reaching the download")
	}

	atCancel := transfers.n.Load()
	time.Sleep(2 * time.Second)
	if after := transfers.n.Load(); after != atCancel {
		t.Errorf("%d TRANSFER_DATA executed after the cancel, want 0", after-atCancel)
	}
}
