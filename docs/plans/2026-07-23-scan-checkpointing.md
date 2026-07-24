# Persistent Scan Checkpointing (Deck #231) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cache remote directory listings in the state DB during a crawl, validated by parent-reported ETags on resume, so a failed `RemoteScan` retries from where it died instead of restarting cold.

**Architecture:** A new `scan_checkpoint` SQLite table (per-account state DB) stores one gzipped-JSON row per listed directory. The engine gains an optional `ScanCheckpoint` hook consulted before each PROPFIND; the agent implements it over the state store and clears rows after clean passes. Spec: `docs/specs/2026-07-23-scan-checkpointing-design.md` — read it before starting.

**Tech Stack:** Go, modernc.org/sqlite (pure-Go, WAL mode), stdlib gzip/json. No frontend work.

## Global Constraints

- **Go-only change.** No `.svelte` edits, no new Wails bindings, no new `App` methods (a bindings regen once broke release 0.1.0.97).
- **`internal/engine` stays log-free** (it has zero `slog`/`log` imports today). All logging lives in `internal/agent`/`internal/state`.
- **`scan_checkpoint` column set is FINAL at first ship.** `CREATE TABLE IF NOT EXISTS` on `Open` is the DB's only migration mechanism; the blob evolves via the `fmt` column only (`cpFormat = 1`; unknown format = cache miss).
- **Empty ETag never matches and is never saved** — on both Load and Save, in both the engine guards and the handle.
- **Mount marker:** `strings.Contains(perms, "M")` — verified live against mainframe: plain dirs are `RGDNVCK`, files `RGDNVW`, the one real mount (`.Collectives`) is `MG`. Use these exact strings in test fixtures.
- Constants: `cpFormat = 1`, age-out **14 days**, delete batch **1000 rows**, `scanWorkers` stays **4**.
- **Commits:** to origin `main` only; message suffix ` [+claude]` and trailer `Co-Authored-By: Claude <noreply@anthropic.com>`. **NEVER push source to the `github` remote** (releases only).
- GUI compile check must use `-H windowsgui` (CLAUDE.md).
- Test packages are internal (`package engine`, `package state`, `package agent`) — matching every existing test in the repo.

---

### Task 1: `ScanOpts` + `PropFinder` refactor (no behavior change) + characterization tests

**Files:**
- Modify: `internal/engine/discovery.go`
- Modify: `internal/agent/agent.go:1703` (computePlan), `:1747` (computePlanScoped), `:2737` (syncRemoteDelta)
- Modify: `internal/agent/escape.go:103`
- Create: `internal/engine/discovery_test.go`

**Interfaces:**
- Consumes: `transport.Entry` (fields: Path, IsDir, Size, ETag, FileID, Checksums, IsEncrypted, Permissions), `*transport.Client.PropFind(ctx, path string, depth int) ([]transport.Entry, error)`.
- Produces: `engine.PropFinder` (interface with that one PropFind method), `engine.ScanOpts{Base, Skip, OnEncrypted, Esc}`, and the single entry point `engine.RemoteScan(ctx context.Context, c PropFinder, root string, opts ScanOpts) (map[string]RemoteState, error)`. `RemoteScanReport` and the old 5-param `RemoteScan` are **deleted**. Task 3 adds a `Checkpoint` field to `ScanOpts`; Tasks 3+ reuse `fakeServer` and the `d`/`fi` fixture helpers defined here.

- [ ] **Step 1: Refactor the engine signature**

In `internal/engine/discovery.go`, replace the two exported functions (lines 52–61: `RemoteScan` wrapper and `RemoteScanReport` signature) with:

```go
// PropFinder is the one slice of transport.Client the remote scan needs — a
// seam so tests can drive crawls from an in-memory tree instead of a WebDAV
// server. *transport.Client satisfies it unchanged.
type PropFinder interface {
	PropFind(ctx context.Context, path string, depth int) ([]transport.Entry, error)
}

// ScanOpts bundles RemoteScan's optional inputs; the zero value is a plain raw
// crawl (no pruning, no filtering, no name decoding).
type ScanOpts struct {
	// Base enables the ETag prune: a directory whose baseline etag matches the
	// server's is reconstructed from the baseline instead of being fetched.
	Base map[string]BaselineState
	// Skip is consulted with each entry's root-relative (decoded) path; a match
	// is omitted AND not descended into. Must use the same predicate as the
	// post-scan ignore filter so it never prunes a path the diff would act on.
	Skip func(rel string) bool
	// OnEncrypted is invoked once per end-to-end encrypted folder encountered;
	// E2EE folders are skipped entirely (opaque ciphertext to us).
	OnEncrypted func(rel string)
	// Esc decodes escaped server names (X.nimboesc -> X) so the remote map is
	// keyed by LOCAL names. Nil when escaping is inactive.
	Esc *Escaper
}

// RemoteScan walks the remote tree under root (a files-root-relative path; ""
// means the whole files root) and returns the current remote state keyed by
// paths *relative to root*, matching the keying used by LocalScan and the
// baseline.
//
// It exploits a Nextcloud invariant for speed: a directory's ETag changes
// whenever anything in its subtree changes. So when a directory's ETag matches
// the baseline, the entire subtree is known-unchanged and is reconstructed from
// the baseline instead of being fetched. (With an empty baseline, e.g. first
// run, it naturally performs a full crawl.)
func RemoteScan(ctx context.Context, c PropFinder, root string, opts ScanOpts) (map[string]RemoteState, error) {
	root = strings.Trim(root, "/")
	base, skip, onEncrypted, esc := opts.Base, opts.Skip, opts.OnEncrypted, opts.Esc
	// ... body of the old RemoteScanReport, UNCHANGED from here down ...
```

Keep the entire old `RemoteScanReport` body verbatim below that line (it already refers to `base`, `skip`, `onEncrypted`, `esc` by those names, so the destructuring line makes the body compile untouched). Delete the old wrapper and the old doc comments they replaced.

- [ ] **Step 2: Update the four call sites**

`internal/agent/agent.go` computePlan (~1693): also hoist the duplicated `PairKey` call while here:

```go
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	base, err := st.LoadBaseline(pk)
	if err != nil {
		return nil, nil, nil, err
	}
	globalIgnore, _ := e.dirs.LoadIgnore()
	ig := engine.NewIgnore(append(append([]string{}, globalIgnore...), p.Excludes...))
	remote, err := engine.RemoteScan(ctx, e.client, p.RemoteRoot, engine.ScanOpts{
		Base: base, Skip: ig.Match, OnEncrypted: e.noteEncrypted, Esc: e.escaper.Load(),
	})
```

(and change the later `pruneDeadBaselines(st, PairKey(...), ...)` / `hashLocal` block at :1715 to use `pk`).

computePlanScoped (~1747):

```go
	subRemote, err := engine.RemoteScan(ctx, e.client, subRoot, engine.ScanOpts{
		Base:        stripScopePrefix(base, scope),
		Skip:        func(rel string) bool { return ig.Match(scope + "/" + rel) },
		OnEncrypted: e.noteEncrypted,
		Esc:         e.escaper.Load(),
	})
```

syncRemoteDelta (~2737):

```go
	remote, err := engine.RemoteScan(ctx, e.client, p.RemoteRoot, engine.ScanOpts{
		Base: base, Skip: ig.Match, OnEncrypted: e.noteEncrypted, Esc: e.escaper.Load(),
	})
```

`internal/agent/escape.go:103`:

```go
		remote, err := engine.RemoteScan(ctx, e.client, p.RemoteRoot, engine.ScanOpts{})
```

- [ ] **Step 3: Compile + existing suite green**

Run: `go build ./... ; go test ./...`
Expected: all packages build, all existing tests PASS (this is the refactor's safety net — behavior is unchanged).

- [ ] **Step 4: Write characterization tests via the new seam**

Create `internal/engine/discovery_test.go`:

```go
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/transport"
)

// fakeServer serves canned depth-1 PROPFIND responses keyed by directory path,
// records every call, and can fail specific directories.
type fakeServer struct {
	mu    sync.Mutex
	dirs  map[string][]transport.Entry
	fail  map[string]error
	calls []string
}

func (f *fakeServer) PropFind(_ context.Context, path string, _ int) ([]transport.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, path)
	if err := f.fail[path]; err != nil {
		return nil, err
	}
	entries, ok := f.dirs[path]
	if !ok {
		return nil, fmt.Errorf("path %q not found", path)
	}
	return entries, nil
}

func (f *fakeServer) callsFor(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == dir {
			n++
		}
	}
	return n
}

// d/fi build entries with permission strings sampled from a live Nextcloud:
// plain dir RGDNVCK, file RGDNVW (mounted dirs, e.g. .Collectives, are "MG").
func d(path, etag string) transport.Entry {
	return transport.Entry{Path: path, IsDir: true, ETag: etag, Permissions: "RGDNVCK"}
}
func fi(path, etag string, size int64) transport.Entry {
	return transport.Entry{Path: path, IsDir: false, ETag: etag, Size: size, Permissions: "RGDNVW"}
}

// newFakeTree builds: root -> a/ (ea), f1 ; a -> a/b/ (eb), a/f2 ; a/b -> a/b/f3.
// Depth-1 responses include the directory's own row first, as the server does.
func newFakeTree() *fakeServer {
	return &fakeServer{
		dirs: map[string][]transport.Entry{
			"":    {d("", "eroot"), d("a", "ea"), fi("f1", "e1", 1)},
			"a":   {d("a", "ea"), d("a/b", "eb"), fi("a/f2", "e2", 2)},
			"a/b": {d("a/b", "eb"), fi("a/b/f3", "e3", 3)},
		},
		fail: map[string]error{},
	}
}

func TestRemoteScanFullCrawl(t *testing.T) {
	f := newFakeTree()
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		isDir bool
		etag  string
	}{
		"a": {true, "ea"}, "f1": {false, "e1"},
		"a/b": {true, "eb"}, "a/f2": {false, "e2"}, "a/b/f3": {false, "e3"},
	}
	if len(out) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(out), len(want), out)
	}
	for p, w := range want {
		r, ok := out[p]
		if !ok || r.IsDir != w.isDir || r.ETag != w.etag {
			t.Errorf("out[%q] = %+v, want isDir=%v etag=%q", p, r, w.isDir, w.etag)
		}
	}
}

func TestRemoteScanBaselinePrune(t *testing.T) {
	f := newFakeTree()
	base := map[string]BaselineState{
		"a/b":    {Path: "a/b", IsDir: true, RemoteETag: "eb", RemoteFileID: "40"},
		"a/b/f3": {Path: "a/b/f3", RemoteETag: "e3", RemoteFileID: "41", LocalSize: 3},
	}
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if f.callsFor("a/b") != 0 {
		t.Errorf("a/b was PROPFINDed despite matching baseline etag")
	}
	if r, ok := out["a/b/f3"]; !ok || r.ETag != "e3" {
		t.Errorf("pruned subtree not reconstructed from baseline: %+v", out["a/b/f3"])
	}
}

func TestRemoteScanSkipPrunesDescent(t *testing.T) {
	f := newFakeTree()
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{
		Skip: func(rel string) bool { return rel == "a" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["a"]; ok {
		t.Error("skipped dir recorded")
	}
	if f.callsFor("a") != 0 {
		t.Error("skipped dir was descended into")
	}
	if _, ok := out["f1"]; !ok {
		t.Error("sibling of skipped dir missing")
	}
}

func TestRemoteScanEncryptedSkipped(t *testing.T) {
	f := newFakeTree()
	f.dirs[""] = append(f.dirs[""], transport.Entry{
		Path: "vault", IsDir: true, ETag: "ev", Permissions: "RGDNVCK", IsEncrypted: true,
	})
	var enc []string
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{
		OnEncrypted: func(rel string) { enc = append(enc, rel) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["vault"]; ok {
		t.Error("E2EE folder recorded")
	}
	if f.callsFor("vault") != 0 {
		t.Error("E2EE folder descended into")
	}
	if len(enc) != 1 || enc[0] != "vault" {
		t.Errorf("onEncrypted = %v, want [vault]", enc)
	}
}

func TestRemoteScanErrorDiscardsAll(t *testing.T) {
	f := newFakeTree()
	f.fail["a/b"] = errors.New("boom")
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{})
	if err == nil {
		t.Fatal("want error")
	}
	if out != nil {
		t.Fatalf("partial result leaked: %v", out)
	}
}
```

(No `Esc` test here: `Escaper` construction is covered by the existing `escape_test.go`; these tests pass `nil`.)

- [ ] **Step 5: Run the new tests**

Run: `go test ./internal/engine/ -run TestRemoteScan -v`
Expected: all 5 PASS (they lock in current behavior through the new seam).

- [ ] **Step 6: Commit**

```bash
git add internal/engine/discovery.go internal/engine/discovery_test.go internal/agent/agent.go internal/agent/escape.go
git commit -m "refactor(engine): ScanOpts + PropFinder seam for RemoteScan [+claude]" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: `scan_checkpoint` table + state accessors + RekeyPair

**Files:**
- Modify: `internal/state/state.go` (schema const :41-59, RekeyPair :513)
- Create: `internal/state/checkpoint.go`
- Create: `internal/state/checkpoint_test.go`

**Interfaces:**
- Consumes: `state.Store` internals (`s.db`, `s.accountID`), existing `Open`.
- Produces (used by Task 4/5):
  - `(*Store).LoadScanDir(pairKey, dir, etag string) (fmtv int, blob []byte, ok bool, err error)`
  - `(*Store).SaveScanDir(pairKey, dir, etag string, fmtv int, blob []byte) error`
  - `(*Store).ClearScanCheckpoint(pairKey string) error`
  - `(*Store).DeleteScanCheckpointBefore(cutoff time.Time) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/state/checkpoint_test.go`:

```go
package state

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

func openCPStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"), "acct1", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func cpCount(t *testing.T, st *Store, pairKey string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM scan_checkpoint WHERE account_id = ? AND pair_key = ?`,
		st.accountID, pairKey,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScanDirRoundtrip(t *testing.T) {
	st := openCPStore(t)
	if err := st.SaveScanDir("pk", "Work/dir_a", "etag1", 1, []byte("blob1")); err != nil {
		t.Fatal(err)
	}
	fmtv, blob, ok, err := st.LoadScanDir("pk", "Work/dir_a", "etag1")
	if err != nil || !ok || fmtv != 1 || string(blob) != "blob1" {
		t.Fatalf("hit = (%d, %q, %v, %v), want (1, blob1, true, nil)", fmtv, blob, ok, err)
	}
	// Wrong etag: miss, nil error (the SQL filters on etag).
	if _, _, ok, err := st.LoadScanDir("pk", "Work/dir_a", "other"); ok || err != nil {
		t.Fatalf("etag mismatch: ok=%v err=%v, want miss", ok, err)
	}
	// Absent row: miss.
	if _, _, ok, err := st.LoadScanDir("pk", "nope", "etag1"); ok || err != nil {
		t.Fatalf("absent: ok=%v err=%v, want miss", ok, err)
	}
	// Upsert replaces: old etag now misses, new hits.
	if err := st.SaveScanDir("pk", "Work/dir_a", "etag2", 1, []byte("blob2")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := st.LoadScanDir("pk", "Work/dir_a", "etag1"); ok {
		t.Fatal("stale etag still hits after upsert")
	}
	if _, blob, ok, _ := st.LoadScanDir("pk", "Work/dir_a", "etag2"); !ok || string(blob) != "blob2" {
		t.Fatal("upserted row missing")
	}
	// LIKE metacharacters in dir_path are inert (exact-match PK lookups only).
	if err := st.SaveScanDir("pk", "100%_done/dir", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := st.LoadScanDir("pk", "100%_done/dir", "e"); !ok {
		t.Fatal("metachar path row missing")
	}
}

func TestClearScanCheckpointBatches(t *testing.T) {
	st := openCPStore(t)
	// >2 batches of 1000 to exercise the bounded-delete loop.
	for i := 0; i < 2500; i++ {
		if err := st.SaveScanDir("pk", fmt.Sprintf("dir/%04d", i), "e", 1, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveScanDir("other", "dir/keep", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearScanCheckpoint("pk"); err != nil {
		t.Fatal(err)
	}
	if n := cpCount(t, st, "pk"); n != 0 {
		t.Fatalf("%d rows left after clear", n)
	}
	if n := cpCount(t, st, "other"); n != 1 {
		t.Fatalf("clear leaked into another pair: %d rows", n)
	}
}

func TestDeleteScanCheckpointBefore(t *testing.T) {
	st := openCPStore(t)
	if err := st.SaveScanDir("pk", "old", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveScanDir("pk", "fresh", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Backdate one row 20 days.
	if _, err := st.db.Exec(
		`UPDATE scan_checkpoint SET saved_at = ? WHERE dir_path = 'old'`,
		time.Now().Add(-20*24*time.Hour).Unix(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteScanCheckpointBefore(time.Now().Add(-14 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := st.LoadScanDir("pk", "old", "e"); ok {
		t.Fatal("aged row survived")
	}
	if _, _, ok, _ := st.LoadScanDir("pk", "fresh", "e"); !ok {
		t.Fatal("fresh row deleted")
	}
}

func TestRekeyPairDropsCheckpointRows(t *testing.T) {
	st := openCPStore(t)
	if err := st.UpsertBaseline("old", engine.BaselineState{Path: "f", RemoteETag: "e"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveScanDir("old", "dir", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := st.RekeyPair("old", "new"); err != nil {
		t.Fatal(err)
	}
	if n := cpCount(t, st, "old"); n != 0 {
		t.Fatal("old-key checkpoint rows survived rekey")
	}
	if n := cpCount(t, st, "new"); n != 0 {
		t.Fatal("checkpoint rows were migrated; they must be dropped (cache, remote root may differ)")
	}
	b, err := st.LoadBaseline("new")
	if err != nil || len(b) != 1 {
		t.Fatalf("baseline did not move: %v %v", b, err)
	}
}

func TestScanDirClosedStore(t *testing.T) {
	st := openCPStore(t)
	st.Close()
	if err := st.SaveScanDir("pk", "dir", "e", 1, []byte("x")); err == nil {
		t.Fatal("Save on closed store: want error")
	}
	if _, _, _, err := st.LoadScanDir("pk", "dir", "e"); err == nil {
		t.Fatal("Load on closed store: want error")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/state/ -run 'ScanDir|ScanCheckpoint|RekeyPairDrops' -v`
Expected: FAIL to compile — `st.SaveScanDir undefined` etc.

- [ ] **Step 3: Implement**

Append to the `schema` const in `internal/state/state.go` (after the `clone_state` table):

```sql
CREATE TABLE IF NOT EXISTS scan_checkpoint (
  account_id TEXT    NOT NULL,
  pair_key   TEXT    NOT NULL,
  dir_path   TEXT    NOT NULL,  -- raw files-root-relative path, exactly as the scan queues it
  etag       TEXT    NOT NULL,  -- parent-reported ETag at queue time (never '')
  fmt        INTEGER NOT NULL,  -- children blob format version; unknown = miss
  saved_at   INTEGER NOT NULL,  -- unix seconds, for the age-out backstop
  children   BLOB    NOT NULL,  -- gzip(JSON []cpEntry), fmt=1 (agent owns the codec)
  PRIMARY KEY (account_id, pair_key, dir_path)
);
```

In `RekeyPair`, after the `for _, table := range ...` loop (state.go:513-520), inside the same transaction:

```go
	// Checkpoint rows are a cache keyed by raw remote paths; the remote root may
	// have changed across the move, so drop them rather than migrate — otherwise
	// they'd be stranded under the old key forever (nothing else ever targets it).
	if _, err := tx.Exec(
		`DELETE FROM scan_checkpoint WHERE account_id=? AND pair_key=?`,
		s.accountID, oldKey,
	); err != nil {
		return fmt.Errorf("rekey scan_checkpoint: %w", err)
	}
```

Create `internal/state/checkpoint.go`:

```go
package state

// Scan-checkpoint rows cache raw remote directory listings during a crawl so a
// failed scan resumes instead of restarting cold (Deck #231; design in
// docs/specs/2026-07-23-scan-checkpointing-design.md). They are a pure cache:
// a row is only reused when its ETag still matches what the parent's listing
// reports, and rows are cleared after a clean pass. Accessors follow the
// CloneStatus pattern — direct DB calls, no s.mu, no cacheEnabled branch — so
// the blobs never enter the RAM baseline cache (low-memory-mode requirement).

import (
	"database/sql"
	"fmt"
	"time"
)

// LoadScanDir returns the stored children blob and format for (pair, dir) iff
// the stored etag equals etag. ok=false with err=nil means no matching row —
// absent, or present under a different etag (the SQL filters on etag, so a
// mismatched row's blob is never even read off disk).
func (s *Store) LoadScanDir(pairKey, dir, etag string) (fmtv int, blob []byte, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT fmt, children FROM scan_checkpoint
		  WHERE account_id = ? AND pair_key = ? AND dir_path = ? AND etag = ?`,
		s.accountID, pairKey, dir, etag,
	).Scan(&fmtv, &blob)
	if err == sql.ErrNoRows {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, fmt.Errorf("load scan checkpoint %q: %w", dir, err)
	}
	return fmtv, blob, true, nil
}

// SaveScanDir records (or replaces) one directory's listing.
func (s *Store) SaveScanDir(pairKey, dir, etag string, fmtv int, blob []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO scan_checkpoint (account_id, pair_key, dir_path, etag, fmt, saved_at, children)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(account_id, pair_key, dir_path) DO UPDATE SET
		   etag=excluded.etag, fmt=excluded.fmt, saved_at=excluded.saved_at, children=excluded.children`,
		s.accountID, pairKey, dir, etag, fmtv, time.Now().Unix(), blob,
	)
	if err != nil {
		return fmt.Errorf("save scan checkpoint %q: %w", dir, err)
	}
	return nil
}

// ClearScanCheckpoint deletes a pair's checkpoint rows, in bounded batches: a
// worst-case clear (a huge failed crawl's rows) as one implicit transaction
// could hold the WAL write lock past another process's 5s busy_timeout (the
// daemon and CLI are separate handles on this file) and fail ITS writes.
func (s *Store) ClearScanCheckpoint(pairKey string) error {
	for {
		res, err := s.db.Exec(
			`DELETE FROM scan_checkpoint WHERE rowid IN (
			   SELECT rowid FROM scan_checkpoint WHERE account_id = ? AND pair_key = ? LIMIT 1000)`,
			s.accountID, pairKey,
		)
		if err != nil {
			return fmt.Errorf("clear scan checkpoint: %w", err)
		}
		if n, _ := res.RowsAffected(); n < 1000 {
			return nil
		}
	}
}

// DeleteScanCheckpointBefore ages out rows saved before cutoff, account-wide —
// the backstop for pairs that never complete a clean pass (a chronic conflict
// would otherwise pin a full-tree row set forever). Bounded batches, as above.
func (s *Store) DeleteScanCheckpointBefore(cutoff time.Time) error {
	for {
		res, err := s.db.Exec(
			`DELETE FROM scan_checkpoint WHERE rowid IN (
			   SELECT rowid FROM scan_checkpoint WHERE account_id = ? AND saved_at < ? LIMIT 1000)`,
			s.accountID, cutoff.Unix(),
		)
		if err != nil {
			return fmt.Errorf("age out scan checkpoint: %w", err)
		}
		if n, _ := res.RowsAffected(); n < 1000 {
			return nil
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/state/ -v`
Expected: all PASS, including the pre-existing state tests (schema addition must not disturb them).

- [ ] **Step 5: Commit**

```bash
git add internal/state/state.go internal/state/checkpoint.go internal/state/checkpoint_test.go
git commit -m "feat(state): scan_checkpoint table + accessors, rekey drops rows [+claude]" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: Engine resume logic

**Files:**
- Modify: `internal/engine/discovery.go`
- Modify: `internal/engine/discovery_test.go` (extend)

**Interfaces:**
- Consumes: `fakeServer`, `d`/`fi`, `newFakeTree` from Task 1.
- Produces: `engine.ScanCheckpoint` interface (below) and a `Checkpoint ScanCheckpoint` field on `ScanOpts` — exactly what Task 4's agent handle implements and Task 5 passes. Also unexported `childrenOnly(entries []transport.Entry, dir string) []transport.Entry`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/engine/discovery_test.go`:

```go
// fakeCheckpoint implements ScanCheckpoint in memory, recording activity.
type cpRow struct {
	etag     string
	children []transport.Entry
}

type fakeCheckpoint struct {
	mu    sync.Mutex
	rows  map[string]cpRow
	loads []string
	saves []string
}

func newFakeCheckpoint() *fakeCheckpoint { return &fakeCheckpoint{rows: map[string]cpRow{}} }

func (f *fakeCheckpoint) Load(dir, expectedETag string) ([]transport.Entry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, dir)
	r, ok := f.rows[dir]
	if !ok || expectedETag == "" || r.etag != expectedETag {
		return nil, false
	}
	return r.children, true
}

func (f *fakeCheckpoint) Save(dir, etag string, children []transport.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves = append(f.saves, dir)
	f.rows[dir] = cpRow{etag: etag, children: children}
}

func (f *fakeCheckpoint) touched(list []string, dir string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range list {
		if d == dir {
			return true
		}
	}
	return false
}

// scan is a shorthand for a checkpointed scan of the whole fake root.
func scan(t *testing.T, f *fakeServer, cp *fakeCheckpoint) map[string]RemoteState {
	t.Helper()
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{Checkpoint: cp})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScanWarmResumeSkipsCachedDirs(t *testing.T) {
	f := newFakeTree()
	cp := newFakeCheckpoint()
	first := scan(t, f, cp) // pre-warm: saves a and a/b (root never saved)
	if cp.touched(cp.saves, "") {
		t.Fatal("root must never be saved (queued with no parent-reported etag)")
	}
	if !cp.touched(cp.saves, "a") || !cp.touched(cp.saves, "a/b") {
		t.Fatalf("expected a and a/b saved, got %v", cp.saves)
	}
	second := scan(t, f, cp)
	if f.callsFor("") != 2 {
		t.Errorf("root fetched %d times, want 2 (always fresh)", f.callsFor(""))
	}
	if f.callsFor("a") != 1 || f.callsFor("a/b") != 1 {
		t.Errorf("cached dirs re-fetched: a=%d a/b=%d, want 1 each", f.callsFor("a"), f.callsFor("a/b"))
	}
	if len(first) != len(second) {
		t.Fatalf("cached scan differs: %v vs %v", first, second)
	}
	for k, v := range first {
		if second[k] != v {
			t.Errorf("cached scan differs at %q: %+v vs %+v", k, v, second[k])
		}
	}
}

func TestScanResumeAfterFailure(t *testing.T) {
	f := newFakeTree()
	f.dirs[""] = append(f.dirs[""], d("c", "ec"))
	f.dirs["c"] = []transport.Entry{d("c", "ec"), fi("c/f4", "e4", 4)}
	f.fail["c"] = errors.New("server buckled")
	cp := newFakeCheckpoint()
	if _, err := RemoteScan(context.Background(), f, "", ScanOpts{Checkpoint: cp}); err == nil {
		t.Fatal("want error from failed dir")
	}
	delete(f.fail, "c")
	callsBefore := map[string]int{}
	cp.mu.Lock()
	cached := make([]string, 0, len(cp.rows))
	for dir := range cp.rows {
		cached = append(cached, dir)
	}
	cp.mu.Unlock()
	for _, dir := range cached {
		callsBefore[dir] = f.callsFor(dir)
	}
	out := scan(t, f, cp)
	// Whatever the failing scan managed to save is not re-fetched on resume.
	for _, dir := range cached {
		if f.callsFor(dir) != callsBefore[dir] {
			t.Errorf("cached dir %q re-fetched on resume", dir)
		}
	}
	if _, ok := out["c/f4"]; !ok {
		t.Error("resumed scan missing the previously-failed subtree")
	}
}

func TestScanRefetchesChangedDir(t *testing.T) {
	f := newFakeTree()
	cp := newFakeCheckpoint()
	scan(t, f, cp) // pre-warm
	// Server-side change: a gets a new etag and a new child.
	f.dirs[""] = []transport.Entry{d("", "eroot2"), d("a", "ea2"), fi("f1", "e1", 1)}
	f.dirs["a"] = []transport.Entry{d("a", "ea2"), d("a/b", "eb"), fi("a/f2", "e2", 2), fi("a/f9", "e9", 9)}
	out := scan(t, f, cp)
	if f.callsFor("a") != 2 {
		t.Errorf("changed dir a fetched %d times total, want 2 (etag mismatch = miss)", f.callsFor("a"))
	}
	if f.callsFor("a/b") != 1 {
		t.Errorf("unchanged a/b re-fetched (%d calls)", f.callsFor("a/b"))
	}
	if _, ok := out["a/f9"]; !ok {
		t.Error("new file in changed dir missing")
	}
	cp.mu.Lock()
	got := cp.rows["a"].etag
	cp.mu.Unlock()
	if got != "ea2" {
		t.Errorf("row for a not overwritten: etag %q, want ea2", got)
	}
}

func TestScanEmptyETagNeverCached(t *testing.T) {
	f := newFakeTree()
	f.dirs[""] = []transport.Entry{d("", "eroot"), d("a", ""), fi("f1", "e1", 1)}
	f.dirs["a"] = []transport.Entry{d("a", ""), fi("a/f2", "e2", 2)}
	delete(f.dirs, "a/b")
	cp := newFakeCheckpoint()
	scan(t, f, cp)
	if cp.touched(cp.loads, "a") || cp.touched(cp.saves, "a") {
		t.Errorf("etag-less dir touched the checkpoint: loads=%v saves=%v", cp.loads, cp.saves)
	}
}

func TestScanMountSubtreeNeverCached(t *testing.T) {
	f := newFakeTree()
	f.dirs[""] = append(f.dirs[""], transport.Entry{Path: "m", IsDir: true, ETag: "em", Permissions: "MG"})
	f.dirs["m"] = []transport.Entry{
		{Path: "m", IsDir: true, ETag: "em", Permissions: "MG"},
		d("m/sub", "es"), // plain perms below the mount point — still excluded by inheritance
	}
	f.dirs["m/sub"] = []transport.Entry{d("m/sub", "es"), fi("m/sub/f5", "e5", 5)}
	cp := newFakeCheckpoint()
	scan(t, f, cp)
	scan(t, f, cp)
	for _, dir := range []string{"m", "m/sub"} {
		if cp.touched(cp.loads, dir) || cp.touched(cp.saves, dir) {
			t.Errorf("mount subtree dir %q touched the checkpoint", dir)
		}
		if f.callsFor(dir) != 2 {
			t.Errorf("mount subtree dir %q fetched %d times, want 2 (always fresh)", dir, f.callsFor(dir))
		}
	}
}

func TestScanLoosenedSkipFetchesFresh(t *testing.T) {
	f := newFakeTree()
	cp := newFakeCheckpoint()
	_, err := RemoteScan(context.Background(), f, "", ScanOpts{
		Checkpoint: cp,
		Skip:       func(rel string) bool { return rel == "a" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cp.touched(cp.saves, "a") {
		t.Fatal("skipped dir was cached")
	}
	out := scan(t, f, cp) // rules loosened: no Skip
	if _, ok := out["a/f2"]; !ok {
		t.Error("previously-skipped subtree missing after rules loosened")
	}
	if f.callsFor("a") != 1 {
		t.Errorf("previously-skipped dir fetched %d times, want exactly 1 (fresh on the second scan)", f.callsFor("a"))
	}
}

func TestScanCachedSelfRowTolerated(t *testing.T) {
	f := newFakeTree()
	fresh, err := RemoteScan(context.Background(), f, "", ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cp := newFakeCheckpoint()
	// Seed a row that (wrongly) still contains the dir's own self row.
	cp.rows["a"] = cpRow{etag: "ea", children: f.dirs["a"]}
	cp.rows["a/b"] = cpRow{etag: "eb", children: f.dirs["a/b"]}
	out := scan(t, f, cp)
	if len(out) != len(fresh) {
		t.Fatalf("self-row replay diverged: %v vs %v", out, fresh)
	}
	for k, v := range fresh {
		if out[k] != v {
			t.Errorf("self-row replay differs at %q", k)
		}
	}
}

func TestChildrenOnly(t *testing.T) {
	entries := []transport.Entry{d("a", "ea"), d("a/b", "eb"), fi("a/f2", "e2", 2)}
	got := childrenOnly(entries, "a")
	if len(got) != 2 || got[0].Path != "a/b" || got[1].Path != "a/f2" {
		t.Fatalf("childrenOnly = %v", got)
	}
	// Self row absent (transport doc: included "when present") — unchanged.
	if got2 := childrenOnly(got, "a"); len(got2) != 2 {
		t.Fatalf("childrenOnly without self row = %v", got2)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/engine/ -run 'TestScan|TestChildrenOnly' -v`
Expected: FAIL to compile — `ScanOpts` has no field `Checkpoint`, `childrenOnly` undefined.

- [ ] **Step 3: Implement**

In `internal/engine/discovery.go`:

Add to `ScanOpts`:

```go
	// Checkpoint persists each directory listing as it is fetched and serves it
	// back on a later attempt when the ETag still matches — so a failed crawl
	// resumes instead of restarting cold. Nil disables checkpointing.
	Checkpoint ScanCheckpoint
```

Add the interface and queue-item type:

```go
// ScanCheckpoint caches raw directory listings across scan attempts (Deck
// #231). Implementations are best-effort: they must swallow their own I/O
// errors (a broken cache only costs re-fetching, never a failed scan).
type ScanCheckpoint interface {
	// Load returns dir's cached children iff the stored etag equals
	// expectedETag. expectedETag == "" must never match.
	Load(dir, expectedETag string) ([]transport.Entry, bool)
	// Save stores dir's children under etag — the etag dir's PARENT listing
	// reported before dir was fetched. That observation strictly precedes the
	// fetch, so an interleaved server change leaves a stale-OLD stored etag:
	// the next attempt misses and refetches. Never a stale hit. etag == ""
	// must be a no-op.
	Save(dir, etag string, children []transport.Entry)
}

// scanItem is one queued directory: its raw files-root-relative path, the ETag
// its parent's listing reported ("" for the root — unknown, so the root is
// always fetched fresh and never saved), and whether checkpointing is disabled
// for this subtree. noCache is set at a mount point ('M' in oc:permissions)
// and inherited by every descendant: external-storage etags are mtime-derived
// — the one place "a change bumps every ancestor's etag" is unreliable — and a
// stale cached listing there could replay as false server-side deletions. The
// baseline prune fails SAFE on a stale etag (it replays the baseline); a
// checkpoint hit replays an old server listing, so it must not run where the
// invariant is weak.
type scanItem struct {
	dir     string
	etag    string
	noCache bool
}
```

Change the queue declaration (`queue = []string{root}` at :77) to:

```go
		queue   = []scanItem{{dir: root}} // directories still to list (LIFO)
```

Replace `process` with (the bookkeeping half is today's code with `dir` → `it.dir` and the struct append):

```go
	cp := opts.Checkpoint

	// process lists one directory (from cache when the checkpoint validates,
	// else the network), records its children, and queues changed
	// subdirectories. PROPFIND and checkpoint I/O run without the lock held;
	// only the in-memory bookkeeping is serialised.
	process := func(it scanItem) {
		var (
			entries []transport.Entry
			err     error
			cached  bool
		)
		if cp != nil && !it.noCache && it.etag != "" {
			entries, cached = cp.Load(it.dir, it.etag)
		}
		if !cached {
			entries, err = c.PropFind(ctx, it.dir, 1)
			if err == nil && cp != nil && !it.noCache && it.etag != "" {
				cp.Save(it.dir, it.etag, childrenOnly(entries, it.dir))
			}
		}

		mu.Lock()
		defer mu.Unlock()
		defer cond.Broadcast() // wake waiters: new work queued, or pending changed
		pending--
		if err != nil {
			if failed == nil {
				failed = err
			}
			return
		}
		for _, e := range entries {
			full := strings.Trim(e.Path, "/")
			if full == it.dir {
				continue // the directory itself, not a child
			}
			rel := relTo(full, root)
			// Decode an escaped server name (X.nimboesc -> X) so the remote map is
			// keyed by LOCAL names; the diff, baseline and ignore rules then all match
			// on X as normal. No-op when escaping is inactive.
			if esc != nil {
				rel, _ = esc.Decode(rel)
			}
			if skip != nil && skip(rel) {
				continue // ignored/excluded — don't record it and don't descend
			}
			if e.IsDir && e.IsEncrypted {
				if onEncrypted != nil {
					onEncrypted(rel)
				}
				continue // E2EE folder — leave untouched on both sides
			}
			out[rel] = RemoteState{
				Path:     rel,
				IsDir:    e.IsDir,
				ETag:     e.ETag,
				FileID:   e.FileID,
				Size:     e.Size,
				SHA1:     parseChecksumSHA1(e.Checksums),
				ReadOnly: e.ServerReadOnly(),
			}
			if !e.IsDir {
				continue
			}
			if b, ok := base[rel]; ok && b.IsDir && b.RemoteETag == e.ETag {
				addBaselineSubtree(out, base, childrenOf, rel) // unchanged subtree — reuse baseline
			} else {
				queue = append(queue, scanItem{
					dir:     full,
					etag:    e.ETag,
					noCache: it.noCache || strings.Contains(e.Permissions, "M"),
				})
				pending++
			}
		}
	}
```

Update the worker loop pop (`dir := queue[len(queue)-1]` at :154-155) to:

```go
				it := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				mu.Unlock()
				process(it)
```

Add the helper:

```go
// childrenOnly strips the directory's own row from a depth-1 response (the
// server includes it "when present") so cached listings hold children only;
// replay tolerates either shape because process() drops an exact-match row.
func childrenOnly(entries []transport.Entry, dir string) []transport.Entry {
	out := make([]transport.Entry, 0, len(entries))
	for _, e := range entries {
		if strings.Trim(e.Path, "/") == dir {
			continue
		}
		out = append(out, e)
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engine/ -v`
Expected: all PASS — the Task 1 characterization tests (unchanged behavior with no checkpoint) and all Task 3 tests.

- [ ] **Step 5: Race check**

Run: `go test ./internal/engine/ -race -run 'TestScan'`
Expected: PASS, no data races (checkpoint I/O runs outside the scan mutex; the fakes carry their own locks).

- [ ] **Step 6: Commit**

```bash
git add internal/engine/discovery.go internal/engine/discovery_test.go
git commit -m "feat(engine): checkpointed remote scan - resume failed crawls from cached listings [+claude]" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: Agent codec + `scanCheckpoint` handle

**Files:**
- Create: `internal/agent/checkpoint.go`
- Create: `internal/agent/checkpoint_test.go`

**Interfaces:**
- Consumes: `engine.ScanCheckpoint` (Task 3), `(*state.Store).LoadScanDir/SaveScanDir` (Task 2), `transport.Entry`.
- Produces (used by Task 5): `newScanCheckpoint(st *state.Store, pk string) *scanCheckpoint` implementing `engine.ScanCheckpoint`, plus `(*scanCheckpoint).stats() (hits, misses, saves int)` and `(*scanCheckpoint).logSummary()`.

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/checkpoint_test.go`:

```go
package agent

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

func openCPTestStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct1", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCPBlobRoundtrip(t *testing.T) {
	children := []transport.Entry{
		{Path: "Work/dir_a/sub", IsDir: true, ETag: "e1", FileID: "10", Permissions: "RGDNVCK"},
		{Path: "Work/dir_a/100%_report.txt", ETag: "e2", FileID: "11", Size: 42,
			Checksums: "SHA1:aa MD5:bb", Permissions: "RGDNVW"},
		{Path: "Work/dir_a/vault", IsDir: true, ETag: "e3", IsEncrypted: true, Permissions: "RGDNVCK"},
	}
	blob, err := encodeCPBlob(children)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeCPBlob("Work/dir_a", blob)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, children) {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, children)
	}
	// Empty listing survives too.
	blob, err = encodeCPBlob(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeCPBlob("x", blob); err != nil || len(got) != 0 {
		t.Fatalf("empty roundtrip: %v %v", got, err)
	}
}

func TestScanCheckpointHitMiss(t *testing.T) {
	st := openCPTestStore(t)
	cp := newScanCheckpoint(st, "pk")
	children := []transport.Entry{{Path: "a/f", ETag: "ef", Size: 1, Permissions: "RGDNVW"}}
	cp.Save("a", "ea", children)
	got, ok := cp.Load("a", "ea")
	if !ok || !reflect.DeepEqual(got, children) {
		t.Fatalf("hit = (%v, %v), want stored children", got, ok)
	}
	if _, ok := cp.Load("a", "other"); ok {
		t.Fatal("etag mismatch must miss")
	}
	if _, ok := cp.Load("nope", "ea"); ok {
		t.Fatal("absent dir must miss")
	}
	hits, misses, saves := cp.stats()
	if hits != 1 || misses != 2 || saves != 1 {
		t.Fatalf("stats = %d/%d/%d, want 1/2/1", hits, misses, saves)
	}
}

func TestScanCheckpointEmptyETagNoops(t *testing.T) {
	st := openCPTestStore(t)
	cp := newScanCheckpoint(st, "pk")
	cp.Save("a", "", []transport.Entry{{Path: "a/f"}})
	if _, _, ok, err := st.LoadScanDir("pk", "a", ""); ok || err != nil {
		t.Fatal("empty-etag Save must not write a row")
	}
	if _, ok := cp.Load("a", ""); ok {
		t.Fatal("empty-etag Load must miss")
	}
	if _, misses, saves := cp.stats(); misses != 0 || saves != 0 {
		t.Fatal("no-op guards must not count as activity")
	}
}

func TestScanCheckpointUnknownFormatMisses(t *testing.T) {
	st := openCPTestStore(t)
	blob, err := encodeCPBlob(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveScanDir("pk", "a", "ea", 99, blob); err != nil {
		t.Fatal(err)
	}
	cp := newScanCheckpoint(st, "pk")
	if _, ok := cp.Load("a", "ea"); ok {
		t.Fatal("unknown fmt must read as a miss")
	}
}

func TestScanCheckpointCorruptBlobMisses(t *testing.T) {
	st := openCPTestStore(t)
	if err := st.SaveScanDir("pk", "a", "ea", cpFormat, []byte("not gzip")); err != nil {
		t.Fatal(err)
	}
	cp := newScanCheckpoint(st, "pk")
	if _, ok := cp.Load("a", "ea"); ok {
		t.Fatal("corrupt blob must read as a miss")
	}
	if cp.tripped() {
		t.Fatal("corrupt row is data, not a store failure — must not trip the fuse")
	}
}

func TestScanCheckpointStickyFuse(t *testing.T) {
	st := openCPTestStore(t)
	st.Close() // every store call now errors
	cp := newScanCheckpoint(st, "pk")
	cp.Save("a", "ea", nil) // must not panic; trips the fuse
	if !cp.tripped() {
		t.Fatal("store error must trip the fuse")
	}
	if _, ok := cp.Load("a", "ea"); ok {
		t.Fatal("tripped handle must miss")
	}
	if _, _, saves := cp.stats(); saves != 0 {
		t.Fatal("failed save counted")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run 'TestCP|TestScanCheckpoint' -v`
Expected: FAIL to compile — `encodeCPBlob`, `newScanCheckpoint` undefined.

- [ ] **Step 3: Implement**

Create `internal/agent/checkpoint.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run 'TestCP|TestScanCheckpoint' -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/checkpoint.go internal/agent/checkpoint_test.go
git commit -m "feat(agent): scan-checkpoint handle - versioned gzip codec over the state DB [+claude]" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5: Agent wiring — scans, clear hooks, age-out

**Files:**
- Modify: `internal/agent/agent.go` (Engine struct ~:134, getStore ~:1811, computePlan ~:1703, cloneRemote ~:1983, RemoveSyncFolder :1031, applyPlan :2472-2480 and :2550-2556, syncRemoteDelta :2737 and :2762-2768)
- Modify: `internal/agent/checkpoint.go` (add the two Engine helpers)
- Modify: `internal/agent/checkpoint_test.go` (extend)

**Interfaces:**
- Consumes: everything Tasks 2–4 produced.
- Produces: `(*Engine).markCheckpointDirty(pk string)` and `(*Engine).clearCheckpoint(st *state.Store, pk string)` — both usable on a zero-value `Engine` (lazy map init; the store is an explicit parameter).

- [ ] **Step 1: Write the failing helper test**

Append to `internal/agent/checkpoint_test.go`:

```go
func TestClearCheckpointGuard(t *testing.T) {
	st := openCPTestStore(t)
	e := &Engine{}
	seed := func() {
		if err := st.SaveScanDir("pk", "dir", "e", cpFormat, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	rowPresent := func() bool {
		_, _, ok, err := st.LoadScanDir("pk", "dir", "e")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// Unknown pair = assume dirty (a previous process life may have left rows).
	seed()
	e.clearCheckpoint(st, "pk")
	if rowPresent() {
		t.Fatal("first clear did not delete")
	}
	// Marked clean now: the guard suppresses the redundant DELETE.
	seed()
	e.clearCheckpoint(st, "pk")
	if !rowPresent() {
		t.Fatal("guard failed: clean pair was re-cleared (want the DELETE skipped)")
	}
	// A scan that saved rows re-dirties; the next clear deletes again.
	e.markCheckpointDirty("pk")
	e.clearCheckpoint(st, "pk")
	if rowPresent() {
		t.Fatal("clear after re-dirty did not delete")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run TestClearCheckpointGuard -v`
Expected: FAIL to compile — `e.clearCheckpoint` undefined.

- [ ] **Step 3: Implement the helpers**

Add to the `Engine` struct in `internal/agent/agent.go`, after the `storeMu/store/storeFinal` block (~:134):

```go
	cpMu    sync.Mutex      // guards cpClean
	cpClean map[string]bool // pair_key -> checkpoint rows known deleted; missing = assume dirty
```

Append to `internal/agent/checkpoint.go`:

```go
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
```

- [ ] **Step 4: Run the helper test**

Run: `go test ./internal/agent/ -run TestClearCheckpointGuard -v`
Expected: PASS.

- [ ] **Step 5: Wire the scans**

`computePlan` (~:1703, as reshaped in Task 1) — create the handle, log, and record dirtiness even when the scan fails (rows saved by a dying scan are exactly the rescue case):

```go
	cp := newScanCheckpoint(st, pk)
	remote, err := engine.RemoteScan(ctx, e.client, p.RemoteRoot, engine.ScanOpts{
		Base: base, Skip: ig.Match, OnEncrypted: e.noteEncrypted, Esc: e.escaper.Load(),
		Checkpoint: cp,
	})
	cp.logSummary()
	if _, _, saves := cp.stats(); saves > 0 {
		e.markCheckpointDirty(pk)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("remote scan: %w", err)
	}
```

`syncRemoteDelta` (~:2737) — identical shape (it already has `pk` at :2719):

```go
	cp := newScanCheckpoint(st, pk)
	remote, err := engine.RemoteScan(ctx, e.client, p.RemoteRoot, engine.ScanOpts{
		Base: base, Skip: ig.Match, OnEncrypted: e.noteEncrypted, Esc: e.escaper.Load(),
		Checkpoint: cp,
	})
	cp.logSummary()
	if _, _, saves := cp.stats(); saves > 0 {
		e.markCheckpointDirty(pk)
	}
	if err != nil {
		return transfer.Stats{}, fmt.Errorf("remote scan: %w", err)
	}
```

`computePlanScoped` and the escape probe stay checkpoint-free (dead code and a rare user-retryable probe, respectively — see the spec's non-goals).

- [ ] **Step 6: Wire the clear hooks (three) + clone start + RemoveSyncFolder + age-out**

**Hook 1 — applyPlan quiet path** (:2472-2480). `base != nil` distinguishes a real full-tree crawl from SyncPaths' stat-built map (same evidence `maintainDirBaselines` uses — SyncPaths must never clear):

```go
	if len(actions) == 0 {
		// Nothing to do — but re-listed dirs still need their etags stamped, or
		// they are re-listed on every future scan (this quiet case is the common
		// steady state: our own transfers stale the ancestor dir etags).
		maintainDirBaselines(st, pk, base, remote, nil)
		if base != nil {
			e.clearCheckpoint(st, pk) // clean pass — the crawl's rescue rows served their purpose
		}
		// A quiet pass must still clear "Scanning…" — nothing else will until the
		// next delta runs, which used to leave the flyout stuck for minutes.
		e.status("Up to date")
		return transfer.Stats{}, nil
	}
```

**Hook 2 — applyPlan executed path** (:2550-2556). Clean = no executor error AND no problems (failed actions + unresolved conflicts). Placed after `maintainDirBaselines` so its batch commit lands first — WAL ordering then makes "cleared but etags unstamped" impossible:

```go
	if err == nil {
		maintainDirBaselines(st, pk, base, remote, problems)
		if base != nil && len(problems) == 0 {
			e.clearCheckpoint(st, pk) // fully clean: every action landed, no conflicts pending
		}
	} else if len(problems) > 0 {
```

**Hook 3 — syncRemoteDelta's no-changes early return** (:2762-2768) — the most common clean outcome; it returns before applyPlan, so hooks 1–2 never see it:

```go
	if len(baseSub) == 0 && len(remoteSub) == 0 {
		slog.Info("remote-delta timing (no changes)",
			"baseline_load", baselineLoad.Round(time.Millisecond),
			"remote_scan", remoteScan.Round(time.Millisecond),
			"baseline_rows", len(base))
		e.clearCheckpoint(st, pk) // a quiet delta is a clean pass
		e.status("Up to date")
		return transfer.Stats{}, nil
	}
```

**Clone start** — in `cloneRemote`, directly after `_ = st.SetCloneStatus(pk, "started")` (:1983):

```go
	// Entering (or resuming) a clone: any checkpoint rows are from a pre-clone
	// life of this pair — there is no baseline worth chaining against, so drop
	// them rather than let stale rescue rows linger for the age-out.
	e.clearCheckpoint(st, pk)
```

**RemoveSyncFolder** (:1031-1056) — replace the whole function with (captures the removed pair so the checkpoint key is built from the pair's own stored fields, not the trimmed argument):

```go
func (e *Engine) RemoveSyncFolder(remoteRoot string, deleteLocal bool) error {
	remoteRoot = strings.Trim(remoteRoot, "/")
	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return err
	}
	var removed *Pair
	out := pairs[:0]
	for _, p := range pairs {
		if strings.Trim(p.RemoteRoot, "/") == remoteRoot {
			p := p
			removed = &p
			continue
		}
		out = append(out, p)
	}
	if err := e.dirs.SavePairs(out); err != nil {
		return err
	}
	if err := e.ReloadPairs(); err != nil { // stops the watcher first
		return err
	}
	if removed != nil {
		// Drop the pair's checkpoint rows — cached dir listings are the big
		// blobs, and nothing else ever targets this pair_key again. Best-effort;
		// the 14-day age-out is the backstop.
		if st, serr := e.getStore(); serr == nil {
			if cerr := st.ClearScanCheckpoint(PairKey(removed.LocalDir, removed.RemoteRoot)); cerr != nil {
				slog.Warn("scan checkpoint clear on remove failed", "err", cerr)
			}
		}
	}
	if deleteLocal && removed != nil {
		return os.RemoveAll(removed.LocalDir)
	}
	return nil
}
```

**Age-out** — in `getStore` (:1811-1815), between the successful `state.Open` and `e.store = st`:

```go
	// Opportunistic age-out: checkpoint rows from crawls that never reached a
	// clean pass expire after 14 days. Runs once per store open (≈ engine
	// lifetime; also covers CLI one-shots).
	if err := st.DeleteScanCheckpointBefore(time.Now().Add(-14 * 24 * time.Hour)); err != nil {
		slog.Warn("scan checkpoint age-out failed", "err", err)
	}
	e.store = st
```

- [ ] **Step 7: Full suite + race**

Run: `go build ./... ; go vet ./... ; go test ./internal/... -race`
Expected: build clean, vet clean, all tests PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/agent/agent.go internal/agent/checkpoint.go internal/agent/checkpoint_test.go
git commit -m "feat(sync): wire scan checkpointing - resume failed crawls, clear on clean pass (Deck #231) [+claude]" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 6: Whole-tree verification

**Files:** none created — verification only.

- [ ] **Step 1: Full test suite**

Run: `go test ./...`
Expected: every package PASS (no frontend involved; `npm run check`'s pre-existing Settings.svelte errors are irrelevant — no `.svelte` file was touched).

- [ ] **Step 2: Build both binaries**

Run (Bash tool):
```bash
go build ./cmd/nimbo && CGO_ENABLED=1 go build -ldflags "-H windowsgui" -o bin/nimbo-gui ./cmd/nimbo-gui
```
Expected: both build. (`-H windowsgui` is mandatory for the GUI per CLAUDE.md; package.ps1/release.ps1 auto-locate gcc if it isn't on PATH — if the CGO build fails to find gcc, check the w64devkit path noted in the build memory rather than changing flags.)

- [ ] **Step 3: Optional live smoke (dev machine, Adam's server)**

Run: `go run ./cmd/nimbo sync` against the configured account, twice.
Expected: pass 1 may log `scan checkpoint reused=… missed=… saved=…` if any directories were re-listed; a warm tree legitimately logs nothing (root-only scan → no saves — the root is never cached by design). No errors either way. This also confirms live that ordinary dirs aren't mount-excluded (already verified by PROPFIND during planning: plain dirs carry `RGDNVCK`, no `M`).

- [ ] **Step 4: Push**

```bash
git push origin main
```
(Retry once on a transient auth failure — known origin quirk. NEVER push to the `github` remote.)

---

## Self-review notes (already applied)

- Spec coverage: schema/final-columns → Task 2; parent-reported-etag + empty-etag + mount rules → Task 3 (guards in both engine and handle — belt and braces per spec); codec/fmt versioning → Task 4; three clear hooks + clone-start + RemoveSyncFolder + RekeyPair + 14-day age-out + dirty hint → Tasks 2/5; observability (agent-side counters, engine log-free) → Task 4/5; testing matrix → Tasks 1–5. Non-goals (clone resume, escape probe, scoped scans, GUI) deliberately absent.
- The `Esc` field is exercised by existing escape tests plus production call sites; characterization tests pass nil (Escaper construction is out of scope here).
- Type consistency: `LoadScanDir` returns `(int, []byte, bool, error)` everywhere it appears (Tasks 2, 4, 5 tests); `stats()` returns `(hits, misses, saves)` in that order at every use.
