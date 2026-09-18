package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeReadCloser is a minimal io.ReadCloser that serves some bytes and then a
// fixed error, so hydrateReadCloser's Read wrapper can be tested without a
// real network reader.
type fakeReadCloser struct {
	data []byte
	err  error
	pos  int
}

func (f *fakeReadCloser) Read(p []byte) (int, error) {
	if f.pos < len(f.data) {
		n := copy(p, f.data[f.pos:])
		f.pos += n
		return n, nil
	}
	return 0, f.err
}

func (f *fakeReadCloser) Close() error { return nil }

func TestHydrateReadCloserReportsAMidStreamFailureOnce(t *testing.T) {
	boom := errors.New("connection reset")
	var reports []error
	r := &hydrateReadCloser{
		ReadCloser: &fakeReadCloser{data: []byte("hello"), err: boom},
		remotePath: "doc.bin",
		report: func(kind, remotePath string, err error) {
			if kind != "download" || remotePath != "doc.bin" {
				t.Errorf("report(%q, %q, %v) unexpected kind/path", kind, remotePath, err)
			}
			reports = append(reports, err)
		},
	}

	if _, err := io.ReadAll(r); !errors.Is(err, boom) {
		t.Fatalf("ReadAll error = %v, want %v", err, boom)
	}
	if len(reports) != 1 {
		t.Fatalf("report called %d times, want exactly 1", len(reports))
	}
	if !errors.Is(reports[0], boom) {
		t.Errorf("reported error = %v, want %v", reports[0], boom)
	}

	// A further Read past the failure must not report a second time.
	if _, err := r.Read(make([]byte, 8)); !errors.Is(err, boom) {
		t.Fatalf("Read after failure = %v, want %v", err, boom)
	}
	if len(reports) != 1 {
		t.Fatalf("report called %d times after a second Read, want still 1", len(reports))
	}
}

func TestHydrateReadCloserNeverReportsCancellation(t *testing.T) {
	reported := false
	r := &hydrateReadCloser{
		ReadCloser: &fakeReadCloser{err: context.Canceled},
		remotePath: "doc.bin",
		report:     func(string, string, error) { reported = true },
	}

	if _, err := io.ReadAll(r); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll error = %v, want %v", err, context.Canceled)
	}
	if reported {
		t.Error("a withdrawn (cancelled) request must not be reported as a failure")
	}
}

func TestHydrateReadCloserDoesNotReportCleanEOF(t *testing.T) {
	reported := false
	r := &hydrateReadCloser{
		ReadCloser: &fakeReadCloser{data: []byte("hi"), err: io.EOF},
		remotePath: "doc.bin",
		report:     func(string, string, error) { reported = true },
	}

	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("ReadAll error = %v, want nil (io.EOF is a clean end)", err)
	}
	if reported {
		t.Error("a clean end of stream (io.EOF) must not be reported as a failure")
	}
}

func TestHydrateReadCloserReportsACleanStreamThatEndsShort(t *testing.T) {
	// The server had fewer bytes than the placeholder promised (the file
	// shrank after it was created, or a proxy clamped the range). The stream
	// ends cleanly, but cfapi cannot complete the request, so the app's open
	// fails — the feed must not claim a successful download.
	var reports []error
	r := &hydrateReadCloser{
		ReadCloser: &fakeReadCloser{data: []byte("only three"), err: io.EOF},
		remotePath: "doc.bin",
		length:     4096,
		report: func(kind, remotePath string, err error) {
			if kind != "download" || remotePath != "doc.bin" {
				t.Errorf("report(%q, %q, %v) unexpected kind/path", kind, remotePath, err)
			}
			reports = append(reports, err)
		},
	}

	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("ReadAll error = %v, want nil (the stream itself ended cleanly)", err)
	}
	if len(reports) != 1 {
		t.Fatalf("report called %d times, want exactly 1", len(reports))
	}
	if got := reports[0].Error(); !strings.Contains(got, "10 of 4096") {
		t.Errorf("reported error = %q, want it to name the byte counts (10 of 4096)", got)
	}
	// Reading past the end must not report again.
	if _, err := r.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read past the end = %v, want io.EOF", err)
	}
	if len(reports) != 1 {
		t.Fatalf("report called %d times after a further Read, want still 1", len(reports))
	}
}

func TestHydrateReadCloserDoesNotReportAStreamThatDeliveredItAll(t *testing.T) {
	reported := false
	body := []byte("exactly ten")
	r := &hydrateReadCloser{
		ReadCloser: &fakeReadCloser{data: body, err: io.EOF},
		remotePath: "doc.bin",
		length:     int64(len(body)),
		report:     func(string, string, error) { reported = true },
	}

	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("ReadAll error = %v, want nil", err)
	}
	if reported {
		t.Error("a stream that delivered every requested byte must not be reported")
	}
}
