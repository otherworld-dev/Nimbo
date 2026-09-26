//go:build windows

package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// fakePlaceholders marks rel paths as placeholders (hydrated unless in dehydrated).
func fakePlaceholders(t *testing.T, hydrated, dehydrated map[string]bool) {
	t.Helper()
	stopFinishedWatchers(t)
	origP, origR := cfIsPlaceholder, cfRevertPlaceholder
	cfIsPlaceholder = func(fi os.FileInfo, full string) bool {
		rel := filepath.ToSlash(filepath.Base(full)) // test trees are flat or use full match below
		_ = rel
		return hydrated[filepath.ToSlash(full)] || dehydrated[filepath.ToSlash(full)]
	}
	t.Cleanup(func() { stopTestWatchers(t); cfIsPlaceholder, cfRevertPlaceholder = origP, origR })
}

func TestScanRevertClassifies(t *testing.T) {
	dir := adoptTree(t,
		"plain.txt:4:0",     // never adopted — untouched
		"hydrated.txt:9:0",  // hydrated placeholder — revert in place
		"stub.txt:9:0:stub", // dehydrated — delete, sync re-downloads
	)
	full := func(rel string) string { return filepath.ToSlash(filepath.Join(dir, rel)) }
	fakePlaceholders(t,
		map[string]bool{full("hydrated.txt"): true},
		map[string]bool{full("stub.txt"): true},
	)
	plan, err := ScanRevert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Hydrated) != 1 || plan.Hydrated[0] != "hydrated.txt" {
		t.Errorf("Hydrated = %v, want [hydrated.txt]", plan.Hydrated)
	}
	if len(plan.Dehydrated) != 1 || plan.Dehydrated[0] != "stub.txt" {
		t.Errorf("Dehydrated = %v, want [stub.txt]", plan.Dehydrated)
	}
	if plan.DownloadBytes != 9 {
		t.Errorf("DownloadBytes = %d, want 9 (the stub's logical size)", plan.DownloadBytes)
	}
}

func TestRevertRunConvertsAndDeletes(t *testing.T) {
	dir := adoptTree(t, "hydrated.txt:9:0", "stub.txt:9:0:stub", "plain.txt:4:0")
	full := func(rel string) string { return filepath.ToSlash(filepath.Join(dir, rel)) }
	fakePlaceholders(t,
		map[string]bool{full("hydrated.txt"): true},
		map[string]bool{full("stub.txt"): true},
	)
	var reverted []string
	cfRevertPlaceholder = func(path string) error {
		reverted = append(reverted, filepath.ToSlash(path))
		return nil
	}
	plan, err := ScanRevert(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seen []int
	res := plan.Run(context.Background(), dir, func(done, total int) {
		if total != 2 {
			t.Errorf("total = %d, want 2", total)
		}
		seen = append(seen, done)
	})
	if res.Reverted != 1 || res.Deleted != 1 || res.Failed != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(reverted) != 1 || reverted[0] != full("hydrated.txt") {
		t.Errorf("reverted %v", reverted)
	}
	if _, err := os.Stat(filepath.Join(dir, "stub.txt")); !os.IsNotExist(err) {
		t.Error("dehydrated stub should have been deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "plain.txt")); err != nil {
		t.Error("plain file must be untouched")
	}
	if len(seen) != 2 {
		t.Errorf("progress calls = %v", seen)
	}
}

func TestRevertRunFailureCountsAndContinues(t *testing.T) {
	dir := adoptTree(t, "a.txt:1:0", "b.txt:1:0")
	full := func(rel string) string { return filepath.ToSlash(filepath.Join(dir, rel)) }
	fakePlaceholders(t, map[string]bool{full("a.txt"): true, full("b.txt"): true}, nil)
	cfRevertPlaceholder = func(path string) error {
		if filepath.Base(path) == "a.txt" {
			return errors.New("filter said no")
		}
		return nil
	}
	plan, err := ScanRevert(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := plan.Run(context.Background(), dir, nil)
	if res.Failed != 1 || res.Reverted != 1 {
		t.Fatalf("res = %+v, want 1 failed 1 reverted", res)
	}
}

func TestRevertRunCancels(t *testing.T) {
	dir := adoptTree(t, "a.txt:1:0", "b.txt:1:0", "c.txt:1:0")
	full := func(rel string) string { return filepath.ToSlash(filepath.Join(dir, rel)) }
	fakePlaceholders(t, map[string]bool{
		full("a.txt"): true, full("b.txt"): true, full("c.txt"): true,
	}, nil)
	var n int
	cfRevertPlaceholder = func(string) error { n++; return nil }
	plan, err := ScanRevert(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	plan.Run(ctx, dir, func(done, _ int) {
		if done == 1 {
			cancel()
		}
	})
	if n != 1 {
		t.Errorf("reverted %d after cancel at 1, want exactly 1", n)
	}
	_ = cfapi.IsDehydrated // keep import honest
}
