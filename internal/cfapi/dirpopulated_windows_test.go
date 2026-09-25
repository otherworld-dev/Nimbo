//go:build windows

package cfapi

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDirPopulatedReadsTheDirectoryPlaceholderState covers the question
// reconcile has to answer about an EMPTY directory: is it empty because
// nothing has ever fetched it, or because it really holds nothing?
//
// A directory placeholder that has never been populated carries
// FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS; the transfer that populates it clears
// that attribute (it passes DISABLE_ON_DEMAND_POPULATION whenever the listing
// succeeded, zero entries included), and the shell then never asks again. A
// plain directory has no such state at all: everything in it is real.
//
// os.ReadDir from this process never populates anything (the filter does not
// issue FETCH_PLACEHOLDERS for the provider's own enumerations - measured
// again here). The populated end is covered by TestMarkDirPopulatedLive and
// TestShellPopulatedDirSettlesLive. (This comment used to say that end could
// not be built from a test because CfUpdatePlaceholder with
// DISABLE_ON_DEMAND_POPULATION was refused outside the callback; that attempt
// passed 0x20, which is REMOVE_FILE_IDENTITY. The flag is 0x10 and works.)
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestDirPopulatedReadsTheDirectoryPlaceholderState(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "dirpopulatedroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	list := func(rel string) []PlaceholderInfo {
		if rel == "" {
			return []PlaceholderInfo{{Name: "seed.txt", Size: 1, ModTime: time.Now(), Identity: []byte("remote/seed.txt")}}
		}
		return []PlaceholderInfo{}
	}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	connKey, err := Mount(root, "NimboDirPopulatedTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "lazy", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/lazy")},
		{Name: "file.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/file.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	lazy := filepath.Join(root, "lazy")

	// The premise: a directory placeholder nobody has populated is online-only
	// in exactly the way a stub file is.
	if attrs, _, aerr := findAttrTag(lazy); aerr != nil {
		t.Fatalf("findAttrTag(lazy): %v", aerr)
	} else if attrs&fileAttrRecallOnDataAccess == 0 {
		t.Fatalf("a never-populated directory placeholder has no RECALL_ON_DATA_ACCESS (attrs=0x%x) - the whole distinction is gone", attrs)
	}
	if pop, perr := DirPopulated(lazy); perr != nil {
		t.Fatalf("DirPopulated(lazy): %v", perr)
	} else if pop {
		t.Error("a never-populated directory placeholder reports populated - reconcile would race the shell's first fetch")
	}

	// Opening it from here changes nothing (the harness limitation above),
	// which is also what makes the attribute a stable answer.
	if _, rerr := os.ReadDir(lazy); rerr != nil {
		t.Fatalf("ReadDir(lazy): %v", rerr)
	}
	time.Sleep(500 * time.Millisecond)
	if pop, perr := DirPopulated(lazy); perr != nil {
		t.Fatalf("DirPopulated(lazy) after open: %v", perr)
	} else if pop {
		t.Error("the directory reports populated after an open that fetched nothing")
	}

	// A plain directory is always "populated": there is nothing to fetch.
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if pop, perr := DirPopulated(plain); perr != nil {
		t.Fatalf("DirPopulated(plain): %v", perr)
	} else if !pop {
		t.Error("a plain directory reports unpopulated - reconcile would never look inside it")
	}

	// A file is not a population question.
	if pop, perr := DirPopulated(filepath.Join(root, "file.bin")); perr != nil {
		t.Fatalf("DirPopulated(file): %v", perr)
	} else if pop {
		t.Error("a file reports populated")
	}

	// And a path that is not there says so rather than guessing.
	if _, perr := DirPopulated(filepath.Join(root, "nope")); perr == nil {
		t.Error("DirPopulated of a missing path returned no error")
	}
}
