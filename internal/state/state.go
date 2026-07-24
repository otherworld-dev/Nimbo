// Package state persists Nimbo's sync baseline — the last-known-synced
// state of every path — in a per-account SQLite database. The baseline is the
// common ancestor the engine's three-way diff compares remote and local against.
//
// It uses the pure-Go modernc.org/sqlite driver (registered as "sqlite") so the
// project cross-compiles without cgo.
package state

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/otherworld/nimbo/internal/engine"

	_ "modernc.org/sqlite"
)

// Store is a handle to one account's sync-state database.
//
// It keeps a resident in-memory copy of each pair's baseline (loaded lazily on
// first access, then kept current as a write-through cache). Re-reading ~720k
// rows from SQLite on every warm sync cost ~5s; serving from RAM makes push/poll
// reconciles fast. The cache is the source of truth for reads while the process
// runs; every write updates RAM and the DB together, so it survives restarts and
// stays correct. A single long-lived Store per engine keeps the cache shared.
type Store struct {
	db        *sql.DB
	accountID string

	cacheEnabled bool                                       // hold baselines resident (vs read per sync)
	mu           sync.Mutex                                 // guards cache
	cache        map[string]map[string]engine.BaselineState // pairKey -> path -> state; entry present == fully loaded
}

// pair_key identifies a sync pair (a specific local folder bound to a specific
// remote folder), not just the remote root — so pointing a different local
// folder at the same remote path starts from a fresh baseline instead of
// mistaking absent files for local deletions.
const schema = `
CREATE TABLE IF NOT EXISTS baseline (
  account_id        TEXT    NOT NULL,
  pair_key          TEXT    NOT NULL,
  path              TEXT    NOT NULL,
  is_dir            INTEGER NOT NULL,
  remote_etag       TEXT    NOT NULL,
  remote_fileid     TEXT    NOT NULL,
  local_size        INTEGER NOT NULL,
  local_mtime_nanos INTEGER NOT NULL,
  content_sha1      TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (account_id, pair_key, path)
);
CREATE TABLE IF NOT EXISTS clone_state (
  account_id TEXT NOT NULL,
  pair_key   TEXT NOT NULL,
  status     TEXT NOT NULL,  -- 'started' (clone in progress) or 'done'
  PRIMARY KEY (account_id, pair_key)
);
CREATE TABLE IF NOT EXISTS scan_checkpoint (
  account_id TEXT    NOT NULL,
  pair_key   TEXT    NOT NULL,
  dir_path   TEXT    NOT NULL,  -- raw files-root-relative path, exactly as the scan queues it
  etag       TEXT    NOT NULL,  -- parent-reported ETag at queue time (never '')
  fmt        INTEGER NOT NULL,  -- children blob format version; unknown = miss
  saved_at   INTEGER NOT NULL,  -- unix seconds, for the age-out backstop
  children   BLOB    NOT NULL,  -- gzip(JSON []cpEntry), fmt=1 (agent owns the codec)
  PRIMARY KEY (account_id, pair_key, dir_path)
);`

// Open opens (creating if needed) the state database at path for the given
// account and ensures the schema exists. cacheBaseline holds each pair's baseline
// resident in RAM (faster warm syncs, larger footprint); false reads from disk per
// sync (smaller footprint — the default "low memory mode").
func Open(path, accountID string, cacheBaseline bool) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// SQLite allows only one writer; a sync (and especially the initial clone) has
	// many workers recording baselines at once. Cap the pool at one connection so
	// those writes queue in Go instead of racing the file lock and failing with
	// SQLITE_BUSY. Writes are serial anyway, and network transfer is the real
	// bottleneck, so this costs nothing.
	db.SetMaxOpenConns(1)
	// WAL improves concurrent read/write behaviour; busy_timeout avoids spurious
	// "database is locked" errors when the daemon and CLI (separate handles) overlap.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		// NORMAL (vs the WAL default FULL) skips an fsync on every commit — safe in
		// WAL (only the last few txns risk loss on power-cut, never corruption), and
		// a big speed-up when recording one baseline per file across a huge clone.
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db, accountID: accountID, cacheEnabled: cacheBaseline, cache: make(map[string]map[string]engine.BaselineState)}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// LoadBaseline returns the baseline for a sync pair, keyed by path relative to
// the pair root. A first run (no rows) yields an empty, non-nil map.
//
// The returned map is the cache's own copy — it is shared and must be treated as
// read-only; change the baseline only through UpsertBaseline/DeleteBaseline.
// Access is safe because reads (the diff) and writes (applying the plan) are
// phased within a serialized sync, and different pairs use different maps.
func (s *Store) LoadBaseline(pairKey string) (map[string]engine.BaselineState, error) {
	if !s.cacheEnabled {
		return s.queryBaseline(pairKey) // low-memory: read from disk, don't retain
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baselineLocked(pairKey)
}

// baselineLocked returns a pair's resident baseline, loading it from the DB on
// first access. Caller must hold s.mu.
func (s *Store) baselineLocked(pairKey string) (map[string]engine.BaselineState, error) {
	if m, ok := s.cache[pairKey]; ok {
		return m, nil
	}
	m, err := s.queryBaseline(pairKey)
	if err != nil {
		return nil, err
	}
	s.cache[pairKey] = m
	return m, nil
}

// queryBaseline reads a pair's full baseline from the database.
func (s *Store) queryBaseline(pairKey string) (map[string]engine.BaselineState, error) {
	rows, err := s.db.Query(
		`SELECT path, is_dir, remote_etag, remote_fileid, local_size, local_mtime_nanos, content_sha1
		   FROM baseline WHERE account_id = ? AND pair_key = ?`,
		s.accountID, pairKey,
	)
	if err != nil {
		return nil, fmt.Errorf("query baseline: %w", err)
	}
	defer rows.Close()

	out := make(map[string]engine.BaselineState)
	for rows.Next() {
		var b engine.BaselineState
		var isDir int
		if err := rows.Scan(&b.Path, &isDir, &b.RemoteETag, &b.RemoteFileID, &b.LocalSize, &b.LocalMTimeNanos, &b.ContentSHA1); err != nil {
			return nil, fmt.Errorf("scan baseline row: %w", err)
		}
		b.IsDir = isDir != 0
		out[b.Path] = b
	}
	return out, rows.Err()
}

// LoadBaselineScoped returns only the baseline rows under a subtree (descendants
// of scope, a pair-relative "/"-separated dir), keyed pair-relative. scope == ""
// falls back to the full load. Served from the resident baseline as a fresh
// subset (safe to mutate; callers don't).
func (s *Store) LoadBaselineScoped(pairKey, scope string) (map[string]engine.BaselineState, error) {
	scope = strings.Trim(scope, "/")
	if scope == "" {
		return s.LoadBaseline(pairKey)
	}
	if !s.cacheEnabled {
		return s.queryBaselineScoped(pairKey, scope)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	full, err := s.baselineLocked(pairKey)
	if err != nil {
		return nil, err
	}
	pre := scope + "/"
	out := make(map[string]engine.BaselineState)
	for k, b := range full {
		if strings.HasPrefix(k, pre) {
			out[k] = b
		}
	}
	return out, nil
}

// queryBaselineScoped reads only a subtree's rows directly from the DB (no-cache).
func (s *Store) queryBaselineScoped(pairKey, scope string) (map[string]engine.BaselineState, error) {
	rows, err := s.db.Query(
		`SELECT path, is_dir, remote_etag, remote_fileid, local_size, local_mtime_nanos, content_sha1
		   FROM baseline WHERE account_id = ? AND pair_key = ? AND path LIKE ? ESCAPE '\'`,
		s.accountID, pairKey, escapeLike(scope)+"/%",
	)
	if err != nil {
		return nil, fmt.Errorf("query scoped baseline: %w", err)
	}
	defer rows.Close()
	out := make(map[string]engine.BaselineState)
	for rows.Next() {
		var b engine.BaselineState
		var isDir int
		if err := rows.Scan(&b.Path, &isDir, &b.RemoteETag, &b.RemoteFileID, &b.LocalSize, &b.LocalMTimeNanos, &b.ContentSHA1); err != nil {
			return nil, fmt.Errorf("scan baseline row: %w", err)
		}
		b.IsDir = isDir != 0
		out[b.Path] = b
	}
	return out, rows.Err()
}

// escapeLike escapes LIKE wildcards so a path with % or _ matches literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// LoadBaselinePaths returns baseline rows for an explicit set of pair-relative
// paths — the targeted-reconcile fast path. Paths with no row are simply omitted.
// Served from the resident baseline as a fresh subset.
func (s *Store) LoadBaselinePaths(pairKey string, paths []string) (map[string]engine.BaselineState, error) {
	if !s.cacheEnabled {
		return s.queryBaselinePaths(pairKey, paths) // low-memory: targeted disk read
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	full, err := s.baselineLocked(pairKey)
	if err != nil {
		return nil, err
	}
	out := make(map[string]engine.BaselineState, len(paths))
	for _, p := range paths {
		if b, ok := full[p]; ok {
			out[p] = b
		}
	}
	return out, nil
}

// queryBaselinePaths reads an explicit set of paths directly from the DB, in
// chunks to stay under SQLite's bound-parameter limit (the no-cache fast path —
// cheap because the set is small, e.g. a local edit's changed files).
func (s *Store) queryBaselinePaths(pairKey string, paths []string) (map[string]engine.BaselineState, error) {
	out := make(map[string]engine.BaselineState, len(paths))
	const chunk = 400
	for i := 0; i < len(paths); i += chunk {
		end := i + chunk
		if end > len(paths) {
			end = len(paths)
		}
		batch := paths[i:end]
		args := make([]any, 0, len(batch)+2)
		args = append(args, s.accountID, pairKey)
		for _, p := range batch {
			args = append(args, p)
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		rows, err := s.db.Query(
			`SELECT path, is_dir, remote_etag, remote_fileid, local_size, local_mtime_nanos, content_sha1
			   FROM baseline WHERE account_id = ? AND pair_key = ? AND path IN (`+ph+`)`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf("query baseline paths: %w", err)
		}
		for rows.Next() {
			var b engine.BaselineState
			var isDir int
			if err := rows.Scan(&b.Path, &isDir, &b.RemoteETag, &b.RemoteFileID, &b.LocalSize, &b.LocalMTimeNanos, &b.ContentSHA1); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan baseline row: %w", err)
			}
			b.IsDir = isDir != 0
			out[b.Path] = b
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// BaselineEmpty reports whether a pair has no baseline yet — i.e. it has never
// completed a sync, so an initial clone (pure download, no diff) is safe.
func (s *Store) BaselineEmpty(pairKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.cache[pairKey]; ok {
		return len(m) == 0, nil
	}
	// Not cached yet: a cheap existence probe, so we don't load the whole baseline
	// just to answer "is it empty?" (the clone-vs-diff decision).
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM baseline WHERE account_id = ? AND pair_key = ? LIMIT 1`,
		s.accountID, pairKey,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return false, err
}

// BaselineCount returns how many paths a pair has in its baseline — the number of
// files/dirs it considers synced. The engine's data-loss guard uses it to gauge
// what fraction of a pair a deletion plan would remove. Served from the resident
// cache when loaded, else a cheap COUNT against the indexed primary key.
func (s *Store) BaselineCount(pairKey string) (int, error) {
	s.mu.Lock()
	if m, ok := s.cache[pairKey]; ok {
		n := len(m)
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM baseline WHERE account_id = ? AND pair_key = ?`,
		s.accountID, pairKey,
	).Scan(&n)
	return n, err
}

// CloneStatus returns a pair's initial-clone state: "" (none yet), "started"
// (clone in progress — resume it), or "done" (use the normal diff path).
func (s *Store) CloneStatus(pairKey string) (string, error) {
	var status string
	err := s.db.QueryRow(
		`SELECT status FROM clone_state WHERE account_id = ? AND pair_key = ?`,
		s.accountID, pairKey,
	).Scan(&status)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return status, err
}

// SetCloneStatus records a pair's initial-clone state.
func (s *Store) SetCloneStatus(pairKey, status string) error {
	_, err := s.db.Exec(
		`INSERT INTO clone_state (account_id, pair_key, status) VALUES (?, ?, ?)
		   ON CONFLICT(account_id, pair_key) DO UPDATE SET status = excluded.status`,
		s.accountID, pairKey, status,
	)
	return err
}

// UpsertBaseline records (or replaces) a single path's synced state. Used by the
// transfer layer (Phase 3) after a successful operation.
func (s *Store) UpsertBaseline(pairKey string, b engine.BaselineState) error {
	isDir := 0
	if b.IsDir {
		isDir = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO baseline
		   (account_id, pair_key, path, is_dir, remote_etag, remote_fileid, local_size, local_mtime_nanos, content_sha1)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(account_id, pair_key, path) DO UPDATE SET
		   is_dir=excluded.is_dir,
		   remote_etag=excluded.remote_etag,
		   remote_fileid=excluded.remote_fileid,
		   local_size=excluded.local_size,
		   local_mtime_nanos=excluded.local_mtime_nanos,
		   content_sha1=excluded.content_sha1`,
		s.accountID, pairKey, b.Path, isDir, b.RemoteETag, b.RemoteFileID, b.LocalSize, b.LocalMTimeNanos, b.ContentSHA1,
	)
	if err != nil {
		return fmt.Errorf("upsert baseline %q: %w", b.Path, err)
	}
	if m, ok := s.cache[pairKey]; ok { // keep the resident copy current
		m[b.Path] = b
	}
	return nil
}

// UpsertBaselineBatch records (or replaces) many paths' synced state in a single
// transaction. Used by the post-pass directory-etag maintenance, where the first
// healing pass can touch thousands of rows — as individual autocommits those
// would each pay a WAL commit.
func (s *Store) UpsertBaselineBatch(pairKey string, rows []engine.BaselineState) error {
	if len(rows) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("upsert baseline batch: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	stmt, err := tx.Prepare(
		`INSERT INTO baseline
		   (account_id, pair_key, path, is_dir, remote_etag, remote_fileid, local_size, local_mtime_nanos, content_sha1)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(account_id, pair_key, path) DO UPDATE SET
		   is_dir=excluded.is_dir,
		   remote_etag=excluded.remote_etag,
		   remote_fileid=excluded.remote_fileid,
		   local_size=excluded.local_size,
		   local_mtime_nanos=excluded.local_mtime_nanos,
		   content_sha1=excluded.content_sha1`)
	if err != nil {
		return fmt.Errorf("upsert baseline batch: %w", err)
	}
	defer stmt.Close()
	for _, b := range rows {
		isDir := 0
		if b.IsDir {
			isDir = 1
		}
		if _, err := stmt.Exec(s.accountID, pairKey, b.Path, isDir, b.RemoteETag, b.RemoteFileID, b.LocalSize, b.LocalMTimeNanos, b.ContentSHA1); err != nil {
			return fmt.Errorf("upsert baseline %q: %w", b.Path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upsert baseline batch: %w", err)
	}
	if m, ok := s.cache[pairKey]; ok { // keep the resident copy current
		for _, b := range rows {
			m[b.Path] = b
		}
	}
	return nil
}

// DeleteBaselineBatch removes many paths' baseline rows in one transaction.
// Used to prune dead rows (paths gone from both sides) after a reconcile.
func (s *Store) DeleteBaselineBatch(pairKey string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete baseline batch: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	stmt, err := tx.Prepare(`DELETE FROM baseline WHERE account_id = ? AND pair_key = ? AND path = ?`)
	if err != nil {
		return fmt.Errorf("delete baseline batch: %w", err)
	}
	defer stmt.Close()
	for _, p := range paths {
		if _, err := stmt.Exec(s.accountID, pairKey, p); err != nil {
			return fmt.Errorf("delete baseline %q: %w", p, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete baseline batch: %w", err)
	}
	if m, ok := s.cache[pairKey]; ok {
		for _, p := range paths {
			delete(m, p)
		}
	}
	return nil
}

// DeleteBaseline removes a single path's baseline row. Used after a delete is
// propagated. It is a no-op if the row is absent.
func (s *Store) DeleteBaseline(pairKey, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`DELETE FROM baseline WHERE account_id = ? AND pair_key = ?  AND path = ?`,
		s.accountID, pairKey, path,
	)
	if err != nil {
		return fmt.Errorf("delete baseline %q: %w", path, err)
	}
	if m, ok := s.cache[pairKey]; ok {
		delete(m, path)
	}
	return nil
}

// RekeyPair moves a pair's entire baseline (and its clone status) from oldKey to
// newKey. The pair_key is derived from the local folder path (see PairKey), so
// MOVING a sync folder changes the key — without this re-key the engine would
// find an empty baseline at the new path and re-clone the whole folder (and read
// the old path as all-deleted). Both the DB rows and the resident cache move
// together, in one transaction. It refuses if newKey already has a baseline, so a
// move can never silently merge into or clobber another pair's state.
func (s *Store) RekeyPair(oldKey, newKey string) error {
	if oldKey == newKey {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin rekey: %w", err)
	}
	defer tx.Rollback()

	var one int
	switch err := tx.QueryRow(
		`SELECT 1 FROM baseline WHERE account_id=? AND pair_key=? LIMIT 1`,
		s.accountID, newKey,
	).Scan(&one); err {
	case nil:
		return fmt.Errorf("destination pair already has a baseline")
	case sql.ErrNoRows:
		// good — destination is empty
	default:
		return fmt.Errorf("probe destination: %w", err)
	}

	for _, table := range []string{"baseline", "clone_state"} {
		if _, err := tx.Exec(
			`UPDATE `+table+` SET pair_key=? WHERE account_id=? AND pair_key=?`,
			newKey, s.accountID, oldKey,
		); err != nil {
			return fmt.Errorf("rekey %s: %w", table, err)
		}
	}
	// Checkpoint rows are a cache keyed by raw remote paths; the remote root may
	// have changed across the move, so drop them rather than migrate — otherwise
	// they'd be stranded under the old key forever (nothing else ever targets it).
	if _, err := tx.Exec(
		`DELETE FROM scan_checkpoint WHERE account_id=? AND pair_key=?`,
		s.accountID, oldKey,
	); err != nil {
		return fmt.Errorf("rekey scan_checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rekey: %w", err)
	}

	// Move the resident cache entry too, so an open Store keeps serving the moved
	// baseline from RAM under the new key.
	if m, ok := s.cache[oldKey]; ok {
		delete(s.cache, oldKey)
		s.cache[newKey] = m
	}
	return nil
}

// DeleteBaselineUnder removes the baseline rows for prefix and everything beneath
// it (prefix and prefix+"/..."). Used when a folder is deselected and its local
// copy is removed: pruning the baseline means a later re-select sees those paths
// as absent-from-baseline and re-downloads them, rather than reading the missing
// local files as deletions to propagate. A blank prefix is a no-op.
func (s *Store) DeleteBaselineUnder(pairKey, prefix string) error {
	prefix = strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/")
	if prefix == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`DELETE FROM baseline WHERE account_id = ? AND pair_key = ? AND (path = ? OR path LIKE ?)`,
		s.accountID, pairKey, prefix, prefix+"/%",
	)
	if err != nil {
		return fmt.Errorf("delete baseline under %q: %w", prefix, err)
	}
	if m, ok := s.cache[pairKey]; ok { // keep the resident copy current
		for p := range m {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				delete(m, p)
			}
		}
	}
	return nil
}
