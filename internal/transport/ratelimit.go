package transport

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// Bandwidth limiting is applied at the transport layer: upload request bodies
// and download response streams are read through a token bucket, so a single
// place governs total transfer rate regardless of how many files are in flight.

// SetLimits configures upload/download caps in KiB/s. Zero or negative means
// unlimited. Safe to call before use; not while transfers are running.
func (c *Client) SetLimits(upKBps, downKBps int) {
	c.upLimiter = newLimiter(upKBps)
	c.downLimiter = newLimiter(downKBps)
}

// newLimiter returns a token-bucket limiter for kbps KiB/s, or nil for unlimited.
func newLimiter(kbps int) *rate.Limiter {
	if kbps <= 0 {
		return nil
	}
	bps := float64(kbps) * 1024
	return rate.NewLimiter(rate.Limit(bps), int(bps)+1) // ~1s burst
}

// limitedReadCloser throttles reads through a limiter while preserving Close.
type limitedReadCloser struct {
	ctx context.Context
	rc  io.ReadCloser
	lim *rate.Limiter
}

func limitReadCloser(ctx context.Context, rc io.ReadCloser, lim *rate.Limiter) io.ReadCloser {
	if lim == nil {
		return rc
	}
	return &limitedReadCloser{ctx: ctx, rc: rc, lim: lim}
}

func (l *limitedReadCloser) Read(p []byte) (int, error) {
	if burst := l.lim.Burst(); len(p) > burst {
		p = p[:burst] // WaitN requires n <= burst
	}
	n, err := l.rc.Read(p)
	if n > 0 {
		_ = l.lim.WaitN(l.ctx, n)
	}
	return n, err
}

func (l *limitedReadCloser) Close() error { return l.rc.Close() }
