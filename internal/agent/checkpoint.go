package agent

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

// cpFormat versions the scan_checkpoint children blob. Rows with an unknown
// format read as misses and are overwritten by the next save — the state DB
// has no column-migration mechanism, so this byte is how the blob can ever
// evolve.
const cpFormat = 1

// cpEntry is the persisted mirror of the transport.Entry fields the remote
// scan consumes. Deliberately NOT transport.Entry itself: explicit short JSON
// tags decouple stored rows from Go field renames, and storing the child's
// NAME instead of its full path roughly halves the blob (a child's path
// repeats the parent prefix; it is reconstructed on load).
type cpEntry struct {
	Name        string `json:"n"`
	IsDir       bool   `json:"d,omitempty"`
	Size        int64  `json:"s,omitempty"`
	ETag        string `json:"e,omitempty"`
	FileID      string `json:"f,omitempty"`
	Checksums   string `json:"c,omitempty"`
	IsEncrypted bool   `json:"x,omitempty"`
	Permissions string `json:"p,omitempty"`
}

// encodeCPBlob serialises a directory's children as gzipped JSON (fmt=1).
func encodeCPBlob(children []transport.Entry) ([]byte, error) {
	rows := make([]cpEntry, 0, len(children))
	for _, e := range children {
		p := strings.Trim(e.Path, "/")
		name := p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			name = p[i+1:]
		}
		rows = append(rows, cpEntry{
			Name: name, IsDir: e.IsDir, Size: e.Size, ETag: e.ETag,
			FileID: e.FileID, Checksums: e.Checksums,
			IsEncrypted: e.IsEncrypted, Permissions: e.Permissions,
		})
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(rows); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeCPBlob reverses encodeCPBlob, re-prefixing each child's path with its
// directory. LastModified/ContentType are zero — the scan never consumes them.
func decodeCPBlob(dir string, blob []byte) ([]transport.Entry, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var rows []cpEntry
	if err := json.NewDecoder(zr).Decode(&rows); err != nil {
		return nil, err
	}
	out := make([]transport.Entry, 0, len(rows))
	for _, r := range rows {
		p := r.Name
		if dir != "" {
			p = dir + "/" + r.Name
		}
		out = append(out, transport.Entry{
			Path: p, IsDir: r.IsDir, Size: r.Size, ETag: r.ETag,
			FileID: r.FileID, Checksums: r.Checksums,
			IsEncrypted: r.IsEncrypted, Permissions: r.Permissions,
		})
	}
	return out, nil
}

// scanCheckpoint adapts the state store to engine.ScanCheckpoint for ONE scan
// of one pair. Fresh per scan: the counters feed the post-scan summary log,
// and the sticky fuse — first store error disables checkpoint I/O for the
// rest of the scan, logged once — keeps a wedged DB from spamming the log or
// slowing a 60k-dir crawl with doomed calls. Best-effort throughout: no error
// here ever fails the scan.
type scanCheckpoint struct {
	st *state.Store
	pk string

	mu     sync.Mutex
	hits   int
	misses int
	saves  int
	broken bool
}

func newScanCheckpoint(st *state.Store, pk string) *scanCheckpoint {
	return &scanCheckpoint{st: st, pk: pk}
}

func (c *scanCheckpoint) Load(dir, expectedETag string) ([]transport.Entry, bool) {
	if expectedETag == "" || c.tripped() {
		return nil, false
	}
	fmtv, blob, ok, err := c.st.LoadScanDir(c.pk, dir, expectedETag)
	if err != nil {
		c.trip("load", err)
		return nil, false
	}
	if !ok || fmtv != cpFormat {
		c.bump(&c.misses)
		return nil, false
	}
	entries, err := decodeCPBlob(dir, blob)
	if err != nil {
		c.bump(&c.misses) // corrupt row is data, not a store failure: the fresh fetch overwrites it
		return nil, false
	}
	c.bump(&c.hits)
	return entries, true
}

func (c *scanCheckpoint) Save(dir, etag string, children []transport.Entry) {
	if etag == "" || c.tripped() {
		return
	}
	blob, err := encodeCPBlob(children)
	if err != nil {
		c.trip("encode", err)
		return
	}
	if err := c.st.SaveScanDir(c.pk, dir, etag, cpFormat, blob); err != nil {
		c.trip("save", err)
		return
	}
	c.bump(&c.saves)
}

func (c *scanCheckpoint) bump(n *int) {
	c.mu.Lock()
	*n++
	c.mu.Unlock()
}

func (c *scanCheckpoint) tripped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.broken
}

func (c *scanCheckpoint) trip(op string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken {
		return
	}
	c.broken = true
	slog.Warn("scan checkpoint disabled for this scan", "op", op, "err", err)
}

// stats returns (hits, misses, saves) for this scan.
func (c *scanCheckpoint) stats() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.saves
}

// logSummary emits one line when the checkpoint did anything this scan.
func (c *scanCheckpoint) logSummary() {
	hits, misses, saves := c.stats()
	if hits > 0 || saves > 0 {
		slog.Info("scan checkpoint", "reused", hits, "missed", misses, "saved", saves)
	}
}

// markCheckpointDirty records that a scan wrote checkpoint rows for this pair,
// so the next clean pass's clear actually runs.
func (e *Engine) markCheckpointDirty(pk string) {
	e.cpMu.Lock()
	defer e.cpMu.Unlock()
	if e.cpClean == nil {
		e.cpClean = make(map[string]bool)
	}
	e.cpClean[pk] = false
}

// clearCheckpoint deletes a pair's checkpoint rows after a clean pass. A
// per-process hint suppresses the redundant DELETE on the frequent quiet
// passes (deltas run every 15s without push): a pair not marked clean is
// assumed dirty, so rows left by a previous process life — or by the CLI's
// separate handle — are cleared on this process's first clean pass.
func (e *Engine) clearCheckpoint(st *state.Store, pk string) {
	e.cpMu.Lock()
	clean := e.cpClean != nil && e.cpClean[pk]
	e.cpMu.Unlock()
	if clean {
		return
	}
	if err := st.ClearScanCheckpoint(pk); err != nil {
		slog.Warn("scan checkpoint clear failed", "err", err)
		return
	}
	e.cpMu.Lock()
	if e.cpClean == nil {
		e.cpClean = make(map[string]bool)
	}
	e.cpClean[pk] = true
	e.cpMu.Unlock()
}
