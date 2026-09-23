package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// etagStore persists the last-synced server ETag per remote path. It is the
// baseline the on-demand write-back uses to detect a conflict: if the server's
// current ETag differs from this baseline when we go to upload a locally-edited
// file, the server was changed too.
type etagStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string
	// keys holds, per file, the content-version key (transport.ContentKey)
	// of the server version its baseline was recorded for, so a new ETag
	// with the same content — files_lock bumps it on every lock and unlock
	// (GitHub #7) — is not read as an edit. Kept in a SEPARATE file so the
	// ETag file stays the flat map older builds read; moves and forgets
	// carry both.
	keys map[string]string
}

func newEtagStore(path string) *etagStore {
	s := &etagStore{path: path, m: map[string]string{}, keys: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
	if b, err := os.ReadFile(s.keysPath()); err == nil {
		_ = json.Unmarshal(b, &s.keys)
	}
	return s
}

// keysPath is the content-key file beside the ETag file:
// vfs-etags.json -> vfs-etags-content.json.
func (s *etagStore) keysPath() string {
	return strings.TrimSuffix(s.path, ".json") + "-content.json"
}

// keysJSONLocked marshals the content keys for a write after unlocking, or
// returns nil when there is nothing to write because nothing changed.
func (s *etagStore) keysJSONLocked(changed bool) []byte {
	if !changed {
		return nil
	}
	b, _ := json.Marshal(s.keys)
	return b
}

func (s *etagStore) writeKeys(b []byte) {
	if b != nil {
		_ = os.WriteFile(s.keysPath(), b, 0o644)
	}
}

// contentKey returns the recorded content-version key for remote ("" = none).
func (s *etagStore) contentKey(remote string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[etagKey(remote)]
}

// setContentKeys records several content keys with a single persist; an empty
// key removes that path's entry.
func (s *etagStore) setContentKeys(m map[string]string) {
	s.mu.Lock()
	changed := false
	for r, k := range m {
		key := etagKey(r)
		if key == "" {
			continue
		}
		if k == "" {
			if _, ok := s.keys[key]; ok {
				delete(s.keys, key)
				changed = true
			}
		} else if s.keys[key] != k {
			s.keys[key] = k
			changed = true
		}
	}
	b := s.keysJSONLocked(changed)
	s.mu.Unlock()
	s.writeKeys(b)
}

func etagKey(remote string) string { return strings.Trim(remote, "/") }

func (s *etagStore) get(remote string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[etagKey(remote)]
}

// set records one baseline and persists.
func (s *etagStore) set(remote, etag string) {
	if etag == "" {
		return
	}
	s.mu.Lock()
	s.m[etagKey(remote)] = etag
	b, _ := json.Marshal(s.m)
	s.mu.Unlock()
	_ = os.WriteFile(s.path, b, 0o644)
}

// del removes one entry and persists (used when a placeholder's remote path goes
// away, e.g. after a rename repoints it).
func (s *etagStore) del(remote string) {
	s.mu.Lock()
	_, hadKey := s.keys[etagKey(remote)]
	delete(s.keys, etagKey(remote))
	kb := s.keysJSONLocked(hadKey)
	if _, ok := s.m[etagKey(remote)]; !ok {
		s.mu.Unlock()
		s.writeKeys(kb)
		return
	}
	delete(s.m, etagKey(remote))
	b, _ := json.Marshal(s.m)
	s.mu.Unlock()
	_ = os.WriteFile(s.path, b, 0o644)
	s.writeKeys(kb)
}

// delUnder removes remote and every entry beneath it, with a single persist.
// Used when a share is detached from the account (Deck #557): a stale entry
// under a vanished path would let the state heal read the parked copy as
// server content.
func (s *etagStore) delUnder(remote string) {
	key := etagKey(remote)
	if key == "" {
		return
	}
	prefix := key + "/"
	s.mu.Lock()
	nk := 0
	for k := range s.keys {
		if k == key || strings.HasPrefix(k, prefix) {
			delete(s.keys, k)
			nk++
		}
	}
	kb := s.keysJSONLocked(nk > 0)
	n := 0
	for k := range s.m {
		if k == key || strings.HasPrefix(k, prefix) {
			delete(s.m, k)
			n++
		}
	}
	if n == 0 {
		s.mu.Unlock()
		s.writeKeys(kb)
		return
	}
	b, _ := json.Marshal(s.m)
	s.mu.Unlock()
	_ = os.WriteFile(s.path, b, 0o644)
	s.writeKeys(kb)
}

// setMany records several baselines with a single persist (for directory
// population / reconcile).
func (s *etagStore) setMany(pairs map[string]string) {
	if len(pairs) == 0 {
		return
	}
	s.mu.Lock()
	for r, e := range pairs {
		if e != "" {
			s.m[etagKey(r)] = e
		}
	}
	b, _ := json.Marshal(s.m)
	s.mu.Unlock()
	_ = os.WriteFile(s.path, b, 0o644)
}

// moveMany carries baselines across a move — each pair is (src, dst) and dst
// takes src's recorded ETag, src is dropped — with a SINGLE persist.
//
// Nextcloud leaves a file's ETag unchanged when it moves it, so the
// placeholder mirrors exactly the version it did before under its new name.
// The batch form exists because every write here rewrites the whole JSON file:
// carrying a moved directory's baselines one descendant at a time meant two
// full rewrites per file — gigabytes of synchronous writes for a large folder,
// inside the move itself.
func (s *etagStore) moveMany(pairs [][2]string) {
	if len(pairs) == 0 {
		return
	}
	s.mu.Lock()
	changed, keysChanged := 0, false
	for _, p := range pairs {
		src, dst := etagKey(p[0]), etagKey(p[1])
		// A blank end, or the same path twice, cannot move anything — and
		// must not be read as "forget src": a dropped baseline costs a
		// spurious conflict the next time that file is edited.
		if src == "" || dst == "" || strings.EqualFold(src, dst) {
			continue
		}
		// The content key travels with the baseline: a move keeps the
		// version, so it still describes the file under its new name.
		// A key already at dst belonged to whatever lived there before and
		// must not vouch for the arriving file.
		if k, ok := s.keys[src]; ok {
			s.keys[dst] = k
			delete(s.keys, src)
			keysChanged = true
		} else if _, ok := s.keys[dst]; ok {
			delete(s.keys, dst)
			keysChanged = true
		}
		e, ok := s.m[src]
		if !ok || e == "" {
			continue // nothing recorded under the old name: nothing to carry
		}
		s.m[dst] = e
		delete(s.m, src)
		changed++
	}
	kb := s.keysJSONLocked(keysChanged)
	if changed == 0 {
		s.mu.Unlock()
		s.writeKeys(kb)
		return
	}
	b, _ := json.Marshal(s.m)
	s.mu.Unlock()
	_ = os.WriteFile(s.path, b, 0o644)
	s.writeKeys(kb)
}

// knownDir reports whether remote is a directory the server is known to have:
// either it has a recorded baseline itself, or some recorded baseline lives
// beneath it. Used by the state heal to decide a plain local directory inside a
// mount is server content that is safe to convert back into a cloud
// placeholder, without a network round trip.
func (s *etagStore) knownDir(remote string) bool {
	key := etagKey(remote)
	if key == "" {
		return false
	}
	prefix := key + "/"
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; ok {
		return true
	}
	for k := range s.m {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}
