package transfer

import "io"

// progReader reports bytes read through it (used to surface upload progress).
type progReader struct {
	r  io.Reader
	fn func(int64)
}

func (p *progReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.fn != nil {
		p.fn(int64(n))
	}
	return n, err
}

// progWriter reports bytes written through it (tee'd into a download's copy).
type progWriter struct{ fn func(int64) }

func (p *progWriter) Write(b []byte) (int, error) {
	if p.fn != nil {
		p.fn(int64(len(b)))
	}
	return len(b), nil
}
