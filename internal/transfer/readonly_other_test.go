//go:build !windows

package transfer

import (
	"os"
	"path/filepath"
	"testing"
)

// Before clearReadOnlyTree only added the write bit, it set every folder it
// walked to 0644, and the delete that followed failed on the folder it had
// just made unsearchable. A folder left like that must still be deletable
// on the next try, not stuck for good.
func TestClearReadOnlyTreeRepairsAFolderLeftUnsearchable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Team")
	old := filepath.Join(root, "old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(old, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(old, 0o755) }) // let TempDir clean up if the test fails

	clearReadOnlyTree(root)

	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("a folder left unsearchable by the old code still can't be deleted: %v", err)
	}
}
