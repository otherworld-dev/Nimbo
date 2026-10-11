package cfapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Deck #701: every FETCH_DATA takes ONE path — streamFetch, tracked, on its
// own context, under the stall watchdog — whether the mount was given a
// streaming or a byte-slice hydration function, and a request with no length
// is answered without opening anything. These stub the two CfExecute calls
// (stubExec, stall_windows_test.go), so they run on every `go test` with no
// sync root.

// A request with no length must be completed without opening a stream: a
// range with no length reads as the whole file (OpenRange sends an un-ranged
// GET for offset 0), and the old loop simply returned, leaving the caller's
// open to the filter's time-out. A zero-length completion is rejected by the
// filter, so it is failed with Length = CF_EOF (see failFetch).
func TestStreamFetchFailsARequestWithNoLengthWithoutOpeningAStream(t *testing.T) {
	for _, length := range []int64{0, -1, -4096} {
		rec := stubExec(t)
		var opened atomic.Int64
		stream := func(context.Context, []byte, int64, int64) (io.ReadCloser, error) {
			opened.Add(1)
			return io.NopCloser(bytes.NewReader(make([]byte, 1<<20))), nil
		}
		(&provider{}).streamFetch(context.Background(), stream, 1, 2, "remote/f.bin", []byte("remote/f.bin"), 8192, length)

		transfers, fails := rec.snapshot()
		if n := opened.Load(); n != 0 {
			t.Errorf("length %d: the stream was opened %d times, want never", length, n)
		}
		if len(transfers) != 0 {
			t.Errorf("length %d: transfers = %v, want none", length, transfers)
		}
		if len(fails) != 1 || fails[0] != [2]int64{8192, cfEOF} {
			t.Errorf("length %d: fails = %v, want the request failed once to EOF: [[8192 %d]]", length, fails, cfEOF)
		}
	}
}

// Same for the byte-slice function: the old loop never ran for a request
// with no length and never completed it either.
func TestSliceHydrateWithNoLengthIsNeverCalled(t *testing.T) {
	rec := stubExec(t)
	var calls atomic.Int64
	hydrate := func([]byte, int64, int64) ([]byte, error) {
		calls.Add(1)
		return []byte{1}, nil
	}
	(&provider{}).streamFetch(context.Background(), sliceStream(hydrate), 1, 2, "remote/f.bin", nil, 0, 0)
	if n := calls.Load(); n != 0 {
		t.Errorf("the byte-slice hydrate was called %d times for a zero-length request", n)
	}
	if _, fails := rec.snapshot(); len(fails) != 1 {
		t.Errorf("fails = %v, want the request failed once", fails)
	}
}

func TestStreamFetchFailsARequestWithNoStream(t *testing.T) {
	rec := stubExec(t)
	(&provider{}).streamFetch(context.Background(), nil, 1, 2, "remote/f.bin", nil, 0, 100)
	if _, fails := rec.snapshot(); len(fails) != 1 || fails[0] != [2]int64{0, 100} {
		t.Errorf("fails = %v, want [[0 100]]", fails)
	}
}

// The byte-slice HydrateFunc, served through streamFetch, still delivers the
// right bytes and is still asked for one transfer chunk at a time.
func TestSliceHydrateIsServedThroughTheStream(t *testing.T) {
	const size = 2*cfTransferChunk + 12345
	content := patternBytes(size)
	got := make([]byte, size)
	var mu sync.Mutex
	var fails [][2]int64
	oldT, oldF := execTransfer, execTransferFail
	execTransfer = func(_, _, offset int64, data []byte) bool {
		mu.Lock()
		defer mu.Unlock()
		copy(got[offset:], data)
		return true
	}
	execTransferFail = func(_, _, offset, length int64) {
		mu.Lock()
		defer mu.Unlock()
		fails = append(fails, [2]int64{offset, length})
	}
	t.Cleanup(func() { execTransfer, execTransferFail = oldT, oldF })

	type call struct{ offset, length int64 }
	var calls []call
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		mu.Lock()
		calls = append(calls, call{offset, length})
		mu.Unlock()
		if string(identity) != "remote/f.bin" {
			return nil, errors.New("wrong identity")
		}
		return content[offset : offset+length], nil
	}
	(&provider{}).streamFetch(context.Background(), sliceStream(hydrate), 1, 2, "remote/f.bin", []byte("remote/f.bin"), 0, size)

	mu.Lock()
	defer mu.Unlock()
	if len(fails) != 0 {
		t.Fatalf("fails = %v, want none", fails)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("the transferred bytes differ from what the byte-slice hydrate served")
	}
	want := []call{{0, cfTransferChunk}, {cfTransferChunk, cfTransferChunk}, {2 * cfTransferChunk, 12345}}
	if len(calls) != len(want) {
		t.Fatalf("hydrate calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("hydrate call %d = %v, want %v", i, calls[i], want[i])
		}
	}
}

// A byte-slice hydrate that fails partway fails the rest of the request, as
// the old loop did; one that hands back nothing (and no error) fails it too.
func TestSliceHydrateFailureFailsTheRest(t *testing.T) {
	const size = 2 * cfTransferChunk
	for name, hydrate := range map[string]HydrateFunc{
		"error": func(_ []byte, offset, length int64) ([]byte, error) {
			if offset > 0 {
				return nil, errors.New("server went away")
			}
			return make([]byte, length), nil
		},
		"empty": func(_ []byte, offset, length int64) ([]byte, error) {
			if offset > 0 {
				return nil, nil
			}
			return make([]byte, length), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := stubExec(t)
			(&provider{}).streamFetch(context.Background(), sliceStream(hydrate), 1, 2, "remote/f.bin", nil, 0, size)
			transfers, fails := rec.snapshot()
			if len(transfers) != 1 || transfers[0] != [2]int64{0, cfTransferChunk} {
				t.Errorf("transfers = %v, want the first chunk only", transfers)
			}
			if len(fails) != 1 || fails[0] != [2]int64{cfTransferChunk, cfTransferChunk} {
				t.Errorf("fails = %v, want the rest failed: [[%d %d]]", fails, cfTransferChunk, cfTransferChunk)
			}
		})
	}
}

// The point of routing the byte-slice function through streamFetch: one that
// hangs (no context, so nothing can interrupt it) used to hold the request
// for good. The stall watchdog now fails it on time.
func TestSliceHydrateThatHangsIsFailedByTheWatchdog(t *testing.T) {
	rec := stubExec(t)
	stallLimit(t, 200*time.Millisecond)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hydrate := func([]byte, int64, int64) ([]byte, error) {
		<-release
		return nil, errors.New("released")
	}
	const size = 1 << 20
	runFetch(t, context.Background(), sliceStream(hydrate), size)
	transfers, fails := rec.snapshot()
	if len(transfers) != 0 {
		t.Errorf("transfers = %v, want none", transfers)
	}
	if len(fails) != 1 || fails[0] != [2]int64{0, size} {
		t.Errorf("fails = %v, want the whole request failed once: [[0 %d]]", fails, size)
	}
}

func TestFetchStreamPrefersTheStream(t *testing.T) {
	var sliceCalls, streamCalls atomic.Int64
	p := &provider{
		hydrate: func([]byte, int64, int64) ([]byte, error) { sliceCalls.Add(1); return []byte{1}, nil },
	}
	read := func() {
		t.Helper()
		f := p.fetchStream()
		if f == nil {
			t.Fatal("fetchStream returned nil with a hydration function in place")
		}
		rc, err := f(context.Background(), nil, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(rc)
		rc.Close()
	}
	read()
	if sliceCalls.Load() != 1 || streamCalls.Load() != 0 {
		t.Fatalf("with only a byte-slice hydrate: slice=%d stream=%d, want 1/0", sliceCalls.Load(), streamCalls.Load())
	}
	p.hydrateStream = func(context.Context, []byte, int64, int64) (io.ReadCloser, error) {
		streamCalls.Add(1)
		return io.NopCloser(bytes.NewReader([]byte{1})), nil
	}
	read()
	if sliceCalls.Load() != 1 || streamCalls.Load() != 1 {
		t.Errorf("with a stream set: slice=%d stream=%d, want 1/1 (the byte-slice func must not be used)", sliceCalls.Load(), streamCalls.Load())
	}
	if (&provider{}).fetchStream() != nil {
		t.Error("fetchStream invented a hydration function for a mount with none")
	}
}

// forgedFetch builds the two structures the filter hands FETCH_DATA, laid out
// through the same field offsets the callback reads (pinned against cfapi.h by
// TestStructLayoutsMatchCfapiH). CF_CALLBACK_INFO is 152 bytes; FetchData's
// parameters need 32. They live in VirtualAlloc'd memory, outside the Go heap
// like the filter's own, so the callback's uintptr arithmetic also passes
// -race's pointer checks.
type forgedFetch struct {
	info, params uintptr
}

const (
	forgedParamsAt   = 256 // params start here in the page; info is at 0
	forgedIdentityAt = 512 // and the identity bytes here
)

func forgeFetch(t *testing.T, connKey, transferKey int64, identity string, offset, length int64) *forgedFetch {
	t.Helper()
	page, err := windows.VirtualAlloc(0, 4096, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if err != nil {
		t.Fatalf("VirtualAlloc: %v", err)
	}
	t.Cleanup(func() { _ = windows.VirtualFree(page, 0, windows.MEM_RELEASE) })
	f := &forgedFetch{info: page, params: page + forgedParamsAt}
	*(*int64)(unsafe.Pointer(f.info + ciConnectionKey)) = connKey
	*(*int64)(unsafe.Pointer(f.info + ciTransferKey)) = transferKey
	if identity != "" {
		id := page + forgedIdentityAt
		copy(unsafe.Slice((*byte)(unsafe.Pointer(id)), len(identity)), identity)
		*(*uintptr)(unsafe.Pointer(f.info + ciFileIdentity)) = id
		*(*uint32)(unsafe.Pointer(f.info + ciFileIdentityLength)) = uint32(len(identity))
	}
	*(*int64)(unsafe.Pointer(f.params + cpRequiredOffset)) = offset
	*(*int64)(unsafe.Pointer(f.params + cpRequiredLength)) = length
	return f
}

func (f *forgedFetch) call() uintptr { return fetchDataCallback(f.info, f.params) }

// The byte-slice path used to run untracked, so CANCEL_FETCH_DATA could not
// stop it. Driven through the real callback: the download is registered under
// its transfer key, a cancel stops it, and a withdrawn request is NOT
// completed (its key is dead).
func TestFetchDataCallbackTracksAByteSliceHydrate(t *testing.T) {
	rec := stubExec(t)
	const connKey, transferKey = int64(0x7F00CAFE), int64(33)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	p := &provider{path: `C:\nowhere`, hydrate: func([]byte, int64, int64) ([]byte, error) {
		started <- struct{}{}
		<-release
		return nil, errors.New("released")
	}}
	providers.Store(connKey, p)
	t.Cleanup(func() { providers.Delete(connKey) })

	if got := forgeFetch(t, connKey, transferKey, "remote/f.bin", 0, 1<<20).call(); got != 0 {
		t.Fatalf("fetchDataCallback returned %d, want 0", got)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the byte-slice hydrate was never called")
	}
	if !p.cancelFetch(transferKey) {
		t.Fatal("the byte-slice download is not tracked under its transfer key, so CANCEL_FETCH_DATA cannot stop it")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.fetchMu.Lock()
		_, still := p.inflight[transferKey]
		p.fetchMu.Unlock()
		if !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancelled download never finished (it is still waiting on the hung hydrate call)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if transfers, fails := rec.snapshot(); len(transfers) != 0 || len(fails) != 0 {
		t.Errorf("transfers = %v, fails = %v; a withdrawn request must not be completed", transfers, fails)
	}
}

// Through the real callback too: a zero-length request is failed without
// calling the hydration function, and a request for a connection with no
// provider is failed rather than ignored.
func TestFetchDataCallbackAnswersRequestsItCannotServe(t *testing.T) {
	rec := stubExec(t)
	const connKey = int64(0x7F00F00D)
	var calls atomic.Int64
	providers.Store(connKey, &provider{path: `C:\nowhere`, hydrateStream: func(context.Context, []byte, int64, int64) (io.ReadCloser, error) {
		calls.Add(1)
		return io.NopCloser(bytes.NewReader(nil)), nil
	}})
	t.Cleanup(func() { providers.Delete(connKey) })

	forgeFetch(t, connKey, 44, "remote/f.bin", 4096, 0).call()
	forgeFetch(t, connKey+1, 55, "remote/g.bin", 0, 8192).call() // no provider for this one

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, fails := rec.snapshot(); len(fails) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, fails := rec.snapshot()
	want := map[[2]int64]bool{{4096, cfEOF}: true, {0, 8192}: true}
	if len(fails) != 2 || !want[fails[0]] || !want[fails[1]] || fails[0] == fails[1] {
		t.Errorf("fails = %v, want both requests failed: [4096 %d] and [0 8192]", fails, cfEOF)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the stream was opened %d times, want never", n)
	}
}
