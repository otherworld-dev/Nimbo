//go:build !windows

package vfs

import "context"

// Leaving VFS is a no-op off Windows; types mirrored so callers compile.

type RevertPlan struct {
	Hydrated      []string
	Dehydrated    []string
	DownloadBytes int64
}

type RevertResult struct {
	Reverted int
	Deleted  int
	Skipped  int
	Failed   int
}

func ScanRevert(string) (RevertPlan, error) { return RevertPlan{}, nil }

func (p RevertPlan) Run(context.Context, string, func(int, int)) RevertResult {
	return RevertResult{}
}
