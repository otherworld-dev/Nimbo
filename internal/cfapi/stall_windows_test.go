package cfapi

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// These drive streamFetch directly with the two CfExecute calls stubbed, so
// they run on every `go test` with no sync root, unlike the live tests in
// hydrate_stream_windows_test.go.

type execRec struct {
	mu        sync.Mutex
	transfers [][2]int64 // offset, length
	fails     [][2]int64 // offset, length
}

func stubExec(t *testing.T) *execRec {
	t.Helper()
	r := &execRec{}
	oldT, oldF := execTransfer, execTransferFail
	execTransfer = func(_, _, offset int64, data []byte) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.transfers = append(r.transfers, [2]int64{offset, int64(len(data))})
		return true
	}
	execTransferFail = func(_, _, offset, length int64) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.fails = append(r.fails, [2]int64{offset, length})
	}
	t.Cleanup(func() { execTransfer, execTransferFail = oldT, oldF })
	return r
}

func (r *execRec) snapshot() (transfers, fails [][2]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][2]int64(nil), r.transfers...), append([][2]int64(nil), r.fails...)
}

func stallLimit(t *testing.T, d time.Duration) {
	t.Helper()
	testReadStallLimit = d
	t.Cleanup(func() { testReadStallLimit = 0 })
}

// silentAfter serves n bytes and then goes quiet without closing, the way a
// connection does when the link is still up but the far end has stopped
// sending. It only returns once its context is cancelled, as an HTTP body does.
type silentAfter struct {
	ctx context.Context
	n   int
}

func (s *silentAfter) Read(p []byte) (int, error) {
	if s.n > 0 {
		if len(p) > s.n {
			p = p[:s.n]
		}
		s.n -= len(p)
		return len(p), nil
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

func (s *silentAfter) Close() error { return nil }

// runFetch runs streamFetch for one request and waits for it to return.
func runFetch(t *testing.T, ctx context.Context, stream HydrateStreamFunc, length int64) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&provider{}).streamFetch(ctx, stream, 1, 2, "remote/f.bin", []byte("remote/f.bin"), 0, length)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streamFetch is still waiting on a stream that has gone silent")
	}
}

// Deck #686 (6). The filter withdraws a request that makes no progress for
// ~60s, but only when another process is waiting on it: Nimbo's own
// hydrations (keeping a pinned file on this device) are never withdrawn. A
// link that stays up but stops sending then held the read, the request and
// the file's pin forever. A stream that delivers nothing for the stall limit
// is cancelled and the request is completed as failed.
func TestStreamFetchFailsARequestWhoseStreamGoesSilent(t *testing.T) {
	rec := stubExec(t)
	stallLimit(t, 200*time.Millisecond)
	const size = 1 << 20
	var streamCtx context.Context
	stream := func(ctx context.Context, _ []byte, _, _ int64) (io.ReadCloser, error) {
		streamCtx = ctx
		return &silentAfter{ctx: ctx, n: 8192}, nil
	}

	runFetch(t, context.Background(), stream, size)

	transfers, fails := rec.snapshot()
	if len(fails) != 1 || fails[0] != [2]int64{0, size} {
		t.Fatalf("fails = %v, want the whole request failed once: [[0 %d]]", fails, size)
	}
	if len(transfers) != 0 {
		t.Errorf("transfers = %v, want none (a piece that cannot be finished is dropped)", transfers)
	}
	if streamCtx.Err() == nil {
		t.Error("the stream's context was not cancelled, so the download is still holding its connection")
	}
}

// The same when the server accepts the connection and never answers: the
// stream is stuck opening rather than reading.
func TestStreamFetchFailsARequestWhoseStreamNeverOpens(t *testing.T) {
	rec := stubExec(t)
	stallLimit(t, 200*time.Millisecond)
	const size = 1 << 20
	stream := func(ctx context.Context, _ []byte, _, _ int64) (io.ReadCloser, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	runFetch(t, context.Background(), stream, size)

	if _, fails := rec.snapshot(); len(fails) != 1 || fails[0] != [2]int64{0, size} {
		t.Fatalf("fails = %v, want the whole request failed once: [[0 %d]]", fails, size)
	}
}

// trickle serves its data a piece at a time with a pause between pieces.
type trickle struct {
	data  []byte
	per   int
	every time.Duration
}

func (r *trickle) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.every)
	n := min(len(p), r.per, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func (r *trickle) Close() error { return nil }

// The limit is on silence, not on the whole download: a slow stream that
// keeps delivering runs for as long as it takes.
func TestStreamFetchLetsASlowButFlowingStreamFinish(t *testing.T) {
	rec := stubExec(t)
	stallLimit(t, 150*time.Millisecond)
	const pieces, per = 20, 4096
	stream := func(ctx context.Context, _ []byte, _, _ int64) (io.ReadCloser, error) {
		return &trickle{data: make([]byte, pieces*per), per: per, every: 30 * time.Millisecond}, nil
	}

	runFetch(t, context.Background(), stream, pieces*per) // ~0.6s in all, four times the limit

	transfers, fails := rec.snapshot()
	if len(fails) != 0 {
		t.Fatalf("fails = %v, want none: the stream never went quiet for the limit", fails)
	}
	var covered int64
	for _, tr := range transfers {
		covered += tr[1]
	}
	if covered != pieces*per {
		t.Fatalf("transfers cover %d bytes, want %d", covered, pieces*per)
	}
}

// A request the filter withdraws is still left alone: its transfer key is
// dead, so completing it would only log a rejection. The watchdog must not
// turn a withdrawal into a failure.
func TestStreamFetchStillLeavesAWithdrawnRequestAlone(t *testing.T) {
	rec := stubExec(t)
	stallLimit(t, 10*time.Second)
	ctx, withdraw := context.WithCancel(context.Background())
	stream := func(ctx context.Context, _ []byte, _, _ int64) (io.ReadCloser, error) {
		return &silentAfter{ctx: ctx, n: 8192}, nil
	}
	time.AfterFunc(50*time.Millisecond, withdraw)

	runFetch(t, ctx, stream, 1<<20)

	if transfers, fails := rec.snapshot(); len(fails) != 0 || len(transfers) != 0 {
		t.Fatalf("transfers = %v, fails = %v, want nothing executed for a withdrawn request", transfers, fails)
	}
}
