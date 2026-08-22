package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalMatchesRemote(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Unix(1700000000, 0)
	mk := func(name string, size int, mt time.Time) os.FileInfo {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	rem := func(size int64, mt time.Time) RemoteState {
		return RemoteState{Size: size, LastModified: mt}
	}

	cases := []struct {
		name   string
		local  os.FileInfo
		remote RemoteState
		want   bool
	}{
		{"exact match", mk("a", 10, t0), rem(10, t0), true},
		{"1s newer still matches", mk("b", 10, t0.Add(time.Second)), rem(10, t0), true},
		{"2s older still matches", mk("c", 10, t0.Add(-2*time.Second)), rem(10, t0), true},
		{"3s adrift does not match", mk("d", 10, t0.Add(3*time.Second)), rem(10, t0), false},
		{"size differs", mk("e", 9, t0), rem(10, t0), false},
		{"hour apart", mk("f", 10, t0.Add(time.Hour)), rem(10, t0), false},
		// A server with no mtime can't be compared, so it never matches — better to
		// transfer than to wrongly assume the local copy is current.
		{"zero remote mtime never matches", mk("g", 10, t0), rem(10, time.Time{}), false},
		{"nil local", nil, rem(10, t0), false},
	}
	for _, c := range cases {
		if got := LocalMatchesRemote(c.local, c.remote); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLocalMatchesRemoteIgnoresDirs(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sub)
	if err != nil {
		t.Fatal(err)
	}
	// Directories have no meaningful size/content comparison — never a match.
	if LocalMatchesRemote(fi, RemoteState{Size: 0, LastModified: fi.ModTime()}) {
		t.Error("a directory must never report as matching a remote file")
	}
}
