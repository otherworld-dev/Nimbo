package agent

import "testing"

func TestPlaceholderAttrs(t *testing.T) {
	const (
		normal     = 0x00000080 // FILE_ATTRIBUTE_NORMAL
		archive    = 0x00000020 // FILE_ATTRIBUTE_ARCHIVE
		sparse     = 0x00000200 // FILE_ATTRIBUTE_SPARSE_FILE
		reparse    = 0x00000400 // FILE_ATTRIBUTE_REPARSE_POINT
		pinned     = 0x00080000 // FILE_ATTRIBUTE_PINNED (cloud file kept local)
		unpinned   = 0x00100000 // FILE_ATTRIBUTE_UNPINNED (cloud file, may dehydrate)
		recallData = 0x00400000 // FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS
		recallOpen = 0x00040000 // FILE_ATTRIBUTE_RECALL_ON_OPEN
		offline    = 0x00001000 // FILE_ATTRIBUTE_OFFLINE
	)
	cases := []struct {
		name  string
		attrs uint32
		want  bool
	}{
		{"plain file", normal, false},
		{"archive", archive, false},
		{"sparse but present", sparse | archive, false},
		{"reparse point that is not a placeholder", reparse | archive, false},
		// A hydrated cloud file is fully on disk — adopting it is correct, so the
		// cloud-file marker attributes alone must NOT read as dehydrated.
		{"hydrated cloud file (pinned)", pinned | archive, false},
		{"cloud file allowed to dehydrate but still present", unpinned | archive, false},
		// The dehydrated markers: contents are not on this disk.
		{"recall on data access", recallData | archive, true},
		{"recall on open", recallOpen | archive, true},
		{"offline", offline | archive, true},
		{"dehydrated cloud file (unpinned + recall)", unpinned | recallData | reparse, true},
		{"no attributes", 0, false},
	}
	for _, c := range cases {
		if got := placeholderAttrs(c.attrs); got != c.want {
			t.Errorf("%s: placeholderAttrs(%#x) = %v, want %v", c.name, c.attrs, got, c.want)
		}
	}
}
