//go:build !windows

package agent

import "sync"

// statusRoots is a no-op off Windows: native sync-state icons come from the
// Cloud Files API, which does not exist there.
type statusRoots struct {
	mu sync.Mutex
}

func newStatusRoots() *statusRoots { return &statusRoots{} }

func (s *statusRoots) enable(string, string, string) error { return nil }
func (s *statusRoots) notifySynced(string, string)         {}
func (s *statusRoots) disable(string)                      {}
