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
}

func newEtagStore(path string) *etagStore {
	s := &etagStore{path: path, m: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
	return s
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
	if _, ok := s.m[etagKey(remote)]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.m, etagKey(remote))
	b, _ := json.Marshal(s.m)
	s.mu.Unlock()
	_ = os.WriteFile(s.path, b, 0o644)
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
