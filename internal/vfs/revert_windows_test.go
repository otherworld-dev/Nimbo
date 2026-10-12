//go:build windows

package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
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
	plan, err := ScanRevert(dir, nil)
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

// Deck #512: an online-only stub under a path live sync ignores must not be
// counted as a download (live mode never syncs it), but it is still deleted
// with the rest, so no provider-less stub is left behind. A HYDRATED file
// under an ignored path keeps its data like any other.
func TestScanRevertSplitsIgnoredStubs(t *testing.T) {
	dir := adoptTree(t,
		"stub.txt:9:0:stub",
		"node_modules/pkg/index.js:7:0:stub",
		"node_modules/pkg/deep/util.js:5:0:stub",
		"node_modules/pkg/opened.js:3:0",
		"src/node_modules.txt:2:0:stub", // name only looks like the pattern
	)
	full := func(rel string) string { return filepath.ToSlash(filepath.Join(dir, rel)) }
	fakePlaceholders(t,
		map[string]bool{full("node_modules/pkg/opened.js"): true},
		map[string]bool{
			full("stub.txt"):                      true,
			full("node_modules/pkg/index.js"):     true,
			full("node_modules/pkg/deep/util.js"): true,
			full("src/node_modules.txt"):          true,
		},
	)
	fate := func(rel string) StubFate {
		if rel == "node_modules" || strings.HasPrefix(rel, "node_modules/") {
			return StubIgnored
		}
		return StubDownloads
	}
	plan, err := ScanRevert(dir, fate)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(plan.Dehydrated)
	sort.Strings(plan.Ignored)
	if want := []string{"src/node_modules.txt", "stub.txt"}; !reflect.DeepEqual(plan.Dehydrated, want) {
		t.Errorf("Dehydrated = %v, want %v", plan.Dehydrated, want)
	}
	if want := []string{"node_modules/pkg/deep/util.js", "node_modules/pkg/index.js"}; !reflect.DeepEqual(plan.Ignored, want) {
		t.Errorf("Ignored = %v, want %v", plan.Ignored, want)
	}
	if want := []string{"node_modules/pkg/opened.js"}; !reflect.DeepEqual(plan.Hydrated, want) {
		t.Errorf("Hydrated = %v, want %v", plan.Hydrated, want)
	}
	if plan.DownloadBytes != 11 {
		t.Errorf("DownloadBytes = %d, want 11 (ignored stubs never download)", plan.DownloadBytes)
	}
	if plan.Total() != 5 {
		t.Errorf("Total = %d, want 5", plan.Total())
	}

	cfRevertPlaceholder = func(string) error { return nil }
	var last, total int
	res := plan.Run(context.Background(), dir, func(done, n int) { last, total = done, n })
	if res.Deleted != 4 || res.Reverted != 1 || res.Failed != 0 {
		t.Fatalf("res = %+v, want 4 deleted 1 reverted", res)
	}
	if last != 5 || total != 5 {
		t.Errorf("progress ended at %d of %d, want 5 of 5", last, total)
	}
	for _, rel := range []string{"node_modules/pkg/index.js", "node_modules/pkg/deep/util.js", "stub.txt"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s: stub should have been deleted", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "pkg", "opened.js")); err != nil {
		t.Error("hydrated file under an ignored path must be kept")
	}
}

// Deck #948: an online-only stub that no sync folder covers is a third case.
// It is not a download (nothing syncs that path) and not "ignored by your
// exclusions" either, so it is counted on its own; Run still deletes it with
// every other stub. Hydrated files keep their data wherever they sit.
func TestScanRevertSplitsUnsyncedStubs(t *testing.T) {
	dir := adoptTree(t,
		"Documents/report.txt:9:0:stub",            // in a sync folder: downloads
		"Documents/node_modules/pkg/a.js:7:0:stub", // in a sync folder, excluded: ignored
		"Photos/cat.jpg:5:0:stub",                  // no sync folder: unsynced
		"Photos/2019/dog.jpg:6:0:stub",             // no sync folder: unsynced
		"node_modules/loose.js:4:0:stub",           // no sync folder beats the rule: unsynced
		"Photos/opened.jpg:3:0",                    // hydrated, outside every folder: kept
		"Documents/opened.txt:2:0",                 // hydrated, inside a folder: kept
		"plain.txt:1:0",                            // never a placeholder
	)
	full := func(rel string) string { return filepath.ToSlash(filepath.Join(dir, rel)) }
	fakePlaceholders(t,
		map[string]bool{full("Photos/opened.jpg"): true, full("Documents/opened.txt"): true},
		map[string]bool{
			full("Documents/report.txt"):            true,
			full("Documents/node_modules/pkg/a.js"): true,
			full("Photos/cat.jpg"):                  true,
			full("Photos/2019/dog.jpg"):             true,
			full("node_modules/loose.js"):           true,
		},
	)
	fate := func(rel string) StubFate {
		if !strings.HasPrefix(rel, "Documents/") {
			return StubUnsynced
		}
		if strings.Contains(rel, "/node_modules/") {
			return StubIgnored
		}
		return StubDownloads
	}
	plan, err := ScanRevert(dir, fate)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(plan.Hydrated)
	sort.Strings(plan.Unsynced)
	if want := []string{"Documents/report.txt"}; !reflect.DeepEqual(plan.Dehydrated, want) {
		t.Errorf("Dehydrated = %v, want %v", plan.Dehydrated, want)
	}
	if want := []string{"Documents/node_modules/pkg/a.js"}; !reflect.DeepEqual(plan.Ignored, want) {
		t.Errorf("Ignored = %v, want %v", plan.Ignored, want)
	}
	if want := []string{"Photos/2019/dog.jpg", "Photos/cat.jpg", "node_modules/loose.js"}; !reflect.DeepEqual(plan.Unsynced, want) {
		t.Errorf("Unsynced = %v, want %v", plan.Unsynced, want)
	}
	if want := []string{"Documents/opened.txt", "Photos/opened.jpg"}; !reflect.DeepEqual(plan.Hydrated, want) {
		t.Errorf("Hydrated = %v, want %v", plan.Hydrated, want)
	}
	if plan.DownloadBytes != 9 {
		t.Errorf("DownloadBytes = %d, want 9 (only the stub a sync folder will fetch)", plan.DownloadBytes)
	}
	if plan.Total() != 7 {
		t.Errorf("Total = %d, want 7 (2 hydrated + 1 download + 1 ignored + 3 unsynced)", plan.Total())
	}

	cfRevertPlaceholder = func(string) error { return nil }
	var last, total int
	res := plan.Run(context.Background(), dir, func(done, n int) { last, total = done, n })
	if res.Deleted != 5 || res.Reverted != 2 || res.Failed != 0 {
		t.Fatalf("res = %+v, want 5 deleted 2 reverted", res)
	}
	if last != 7 || total != 7 {
		t.Errorf("progress ended at %d of %d, want 7 of 7", last, total)
	}
	for _, rel := range []string{
		"Documents/report.txt", "Documents/node_modules/pkg/a.js",
		"Photos/cat.jpg", "Photos/2019/dog.jpg", "node_modules/loose.js",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s: stub should have been deleted", rel)
		}
	}
	for _, rel := range []string{"Photos/opened.jpg", "Documents/opened.txt", "plain.txt"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s: must be kept: %v", rel, err)
		}
	}
}

// Deck #948, as seen on the test VM: with no sync folder to restore, every
// online-only stub is unsynced. Nothing is forecast as a download.
func TestScanRevertNoSyncFolders(t *testing.T) {
	dir := adoptTree(t, "a.txt:9:0:stub", "sub/b.txt:7:0:stub", "opened.txt:3:0")
	full := func(rel string) string { return filepath.ToSlash(filepath.Join(dir, rel)) }
	fakePlaceholders(t,
		map[string]bool{full("opened.txt"): true},
		map[string]bool{full("a.txt"): true, full("sub/b.txt"): true},
	)
	plan, err := ScanRevert(dir, func(string) StubFate { return StubUnsynced })
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(plan.Unsynced)
	if want := []string{"a.txt", "sub/b.txt"}; !reflect.DeepEqual(plan.Unsynced, want) {
		t.Errorf("Unsynced = %v, want %v", plan.Unsynced, want)
	}
	if len(plan.Dehydrated) != 0 || len(plan.Ignored) != 0 || plan.DownloadBytes != 0 {
		t.Errorf("nothing may be forecast as a download or as ignored: %+v", plan)
	}
	if len(plan.Hydrated) != 1 || plan.Total() != 3 {
		t.Errorf("Hydrated = %v, Total = %d, want 1 and 3", plan.Hydrated, plan.Total())
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
	plan, err := ScanRevert(dir, nil)
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
	plan, err := ScanRevert(dir, nil)
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
	plan, err := ScanRevert(dir, nil)
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
