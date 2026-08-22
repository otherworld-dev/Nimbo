//go:build !windows

package vfs

import (
	"context"

	"github.com/otherworld/nimbo/internal/engine"
)

// Adopting an existing folder is a no-op outside Windows: it exists to integrate
// files into Cloud Files placeholders, which are Windows-only. The types are
// mirrored so cross-platform callers compile.

// Action is what adopt would do with one pre-existing local entry.
type Action int

const (
	ActionKeep Action = iota
	ActionConflict
	ActionUpload
	ActionReplace
)

func (a Action) String() string { return "unsupported" }

// Entry is one classified local file.
type Entry struct {
	Rel        string `json:"rel"`
	Action     Action `json:"action"`
	Size       int64  `json:"size"`
	MTimeNanos int64  `json:"mtimeNanos"`
	RemoteRel  string `json:"remoteRel"`
}

// UploadItem is one pending upload (local rel + server-side name).
type UploadItem struct {
	Rel       string
	RemoteRel string
}

// Plan is a scan's result.
type Plan struct {
	Entries []Entry `json:"entries"`
}

// Counts totals the entries per action.
func (p Plan) Counts() map[Action]int { return map[Action]int{} }

// UploadBytes is how much would be sent to the server.
func (p Plan) UploadBytes() int64 { return 0 }

// ApplyResult reports what phase A did and what remains for phase B.
type ApplyResult struct {
	Uploads  []UploadItem
	Kept     int
	Replaced int
	Renamed  int
	Skipped  int
	Failed   int
}

// AdoptOps are the side effects UploadPending needs (unused off Windows).
type AdoptOps struct {
	Upload func(rel, remoteRel string) error
	Log    func(format string, args ...any)
}

// Scan returns an empty plan off Windows; callers are gated by cfapi.Supported.
func Scan(string, map[string]engine.RemoteState, func(rel string) bool, func(rel string) string) (Plan, error) {
	return Plan{}, nil
}

// Apply does nothing off Windows.
func (p Plan) Apply(context.Context, string, string, func(int, int)) ApplyResult {
	return ApplyResult{}
}

// UploadPending does nothing off Windows.
func UploadPending(string, string, []UploadItem, AdoptOps) int { return 0 }
