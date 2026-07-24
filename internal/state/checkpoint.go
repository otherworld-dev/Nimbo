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
