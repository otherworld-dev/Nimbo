package cli

import (
	"reflect"
	"testing"
)

func file(size int64) repairEntry { return repairEntry{size: size} }
func dir() repairEntry            { return repairEntry{isDir: true} }

func TestClassifyRepair(t *testing.T) {
	local := map[string]repairEntry{
		"a.txt":          file(10),
		"same.txt":       file(20),
		"big.bin":        file(100),
		"docs":           dir(),
		"docs/note.md":   file(5),
		"newdir":         dir(),
		"newdir/only.go": file(7),
	}
	server := map[string]repairEntry{
		"same.txt":     file(20),  // matches
		"big.bin":      file(64),  // size mismatch
		"docs":         dir(),     // matches
		"docs/note.md": file(5),   // matches
		"serveronly.x": file(3),   // extra (not local)
		"a.txt":        dir(),     // local file vs server dir -> missing (type clash)
	}

	p := classifyRepair(local, server)

	if got := p.files; got != 5 { // a.txt, same.txt, big.bin, docs/note.md, newdir/only.go
		t.Errorf("files = %d, want 5", got)
	}
	if got := p.dirs; got != 2 { // docs, newdir
		t.Errorf("dirs = %d, want 2", got)
	}
	if want := []string{"a.txt", "newdir/only.go"}; !reflect.DeepEqual(p.missingFiles, want) {
		t.Errorf("missingFiles = %v, want %v", p.missingFiles, want)
	}
	if want := []string{"newdir"}; !reflect.DeepEqual(p.missingDirs, want) {
		t.Errorf("missingDirs = %v, want %v", p.missingDirs, want)
	}
	if want := []string{"big.bin"}; !reflect.DeepEqual(p.mismatched, want) {
		t.Errorf("mismatched = %v, want %v", p.mismatched, want)
	}
	if got := p.mismSize["big.bin"]; got != [2]int64{100, 64} {
		t.Errorf("mismSize[big.bin] = %v, want [100 64]", got)
	}
	if want := []string{"serveronly.x"}; !reflect.DeepEqual(p.extra, want) {
		t.Errorf("extra = %v, want %v", p.extra, want)
	}
}

// TestClassifyRepair_NeverPlansDeletes is the safety guarantee: even with files
// only on the server, the plan has no delete concept — extras are reported, and
// nothing about the plan removes server content.
func TestClassifyRepair_NeverPlansDeletes(t *testing.T) {
	local := map[string]repairEntry{"keep.txt": file(1)}
	server := map[string]repairEntry{
		"keep.txt": file(1),
		"a/b.txt":  file(2),
		"a/c.txt":  file(3),
	}
	p := classifyRepair(local, server)
	if len(p.missingFiles)+len(p.missingDirs)+len(p.mismatched) != 0 {
		t.Errorf("nothing should need uploading, got %+v", p)
	}
	if want := []string{"a/b.txt", "a/c.txt"}; !reflect.DeepEqual(p.extra, want) {
		t.Errorf("server-only files should be reported as extra, got %v", p.extra)
	}
}
