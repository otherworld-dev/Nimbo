package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

func TestDecideCloneFile(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Unix(1700000000, 0)
	// mk writes a file of the given size with the given mtime and returns its stat.
	mk := func(name string, size int, mt time.Time) os.FileInfo {
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
	rem := func(size int64, mt time.Time) engine.RemoteState {
		return engine.RemoteState{Size: size, LastModified: mt}
	}

	cases := []struct {
		name     string
		takeover bool
		local    os.FileInfo
		remote   engine.RemoteState
		want     cloneDecision
	}{
		{"absent -> download", false, nil, rem(10, t0), cloneDownload},
		{"absent (takeover) -> download", true, nil, rem(10, t0), cloneDownload},
		{"resume size match -> adopt", false, mk("a", 10, t0), rem(10, t0), cloneAdopt},
		{"resume size mismatch -> refetch", false, mk("b", 9, t0), rem(10, t0), cloneDownload},
		{"takeover exact match -> adopt", true, mk("c", 10, t0), rem(10, t0), cloneAdopt},
		{"takeover within 2s -> adopt", true, mk("f", 10, t0.Add(time.Second)), rem(10, t0), cloneAdopt},
		// The data-loss-prevention cases: a differing local file is SKIPPED, never
		// downloaded over (which would destroy the user's version).
		{"takeover mtime differs -> skip", true, mk("d", 10, t0.Add(time.Hour)), rem(10, t0), cloneSkip},
		{"takeover size differs -> skip", true, mk("e", 5, t0), rem(10, t0), cloneSkip},
	}
	for _, c := range cases {
		if got := decideCloneFile(c.takeover, c.local, c.remote); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
