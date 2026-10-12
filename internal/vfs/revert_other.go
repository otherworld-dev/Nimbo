//go:build !windows

package vfs

import "context"

// Leaving VFS is a no-op off Windows; types mirrored so callers compile.

type RevertPlan struct {
	Hydrated      []string
	Dehydrated    []string
	Ignored       []string
	Unsynced      []string
	DownloadBytes int64
}

type RevertResult struct {
	Reverted int
	Deleted  int
	Skipped  int
	Failed   int
}

func ScanRevert(string, func(string) StubFate) (RevertPlan, error) { return RevertPlan{}, nil }

func (p RevertPlan) Total() int {
	return len(p.Hydrated) + len(p.Dehydrated) + len(p.Ignored) + len(p.Unsynced)
}

func (p RevertPlan) Run(context.Context, string, func(int, int)) RevertResult {
	return RevertResult{}
}
