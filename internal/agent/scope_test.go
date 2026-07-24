package agent

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

func TestScopesFor(t *testing.T) {
	root := filepath.Join("C:", "sync")
	j := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }

	tests := []struct {
		name    string
		changed []string
		want    []string
		wantOK  bool
	}{
		{"single nested file", []string{j("a", "b", "c.txt")}, []string{"a/b"}, true},
		{"two files same dir", []string{j("a", "b", "c.txt"), j("a", "b", "d.txt")}, []string{"a/b"}, true},
		{"two distinct branches", []string{j("a", "x.txt"), j("b", "y.txt")}, []string{"a", "b"}, true},
		{"ancestor covers descendant", []string{j("a", "x.txt"), j("a", "b", "y.txt")}, []string{"a"}, true},
		{"root-level file forces full", []string{j("top.txt")}, nil, false},
		{"empty forces full", nil, nil, false},
		{"path outside pair forces full", []string{filepath.Join("C:", "elsewhere", "z.txt")}, nil, false},
	}
	for _, tt := range tests {
		got, ok := scopesFor(root, tt.changed)
		if ok != tt.wantOK {
			t.Errorf("%s: ok=%v want %v", tt.name, ok, tt.wantOK)
			continue
		}
		if !ok {
			continue
		}
		sort.Strings(got)
		want := append([]string(nil), tt.want...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", tt.name, got, want)
		}
	}
}

func TestScopesForCapFallsBackToFull(t *testing.T) {
	root := filepath.Join("C:", "sync")
	var changed []string
	for i := 0; i < maxScopes+5; i++ {
		changed = append(changed, filepath.Join(root, "dir"+string(rune('a'+i%26))+string(rune('0'+i)), "f.txt"))
	}
	if _, ok := scopesFor(root, changed); ok {
		t.Errorf("expected full-sync fallback when scopes exceed cap")
	}
}

func TestScopePrefixRoundTrip(t *testing.T) {
	// stripScopePrefix drops the scope prefix (and skips siblings outside scope).
	base := map[string]engine.BaselineState{
		"photos/2024/a.jpg": {Path: "photos/2024/a.jpg", RemoteETag: "e1"},
		"photos/2024/sub":   {Path: "photos/2024/sub", IsDir: true, RemoteETag: "e2"},
		"photos/other.txt":  {Path: "photos/other.txt", RemoteETag: "e3"},
	}
	stripped := stripScopePrefix(base, "photos/2024")
	if len(stripped) != 2 {
		t.Fatalf("expected 2 entries under scope, got %d: %v", len(stripped), stripped)
	}
	if b, ok := stripped["a.jpg"]; !ok || b.Path != "a.jpg" || b.RemoteETag != "e1" {
		t.Errorf("a.jpg not re-keyed correctly: %+v ok=%v", b, ok)
	}
	if _, ok := stripped["other.txt"]; ok {
		t.Errorf("sibling outside scope should be excluded")
	}

	// addScopePrefix lifts subtree-relative keys back to pair-relative.
	rem := map[string]engine.RemoteState{
		"a.jpg": {Path: "a.jpg", ETag: "e1"},
		"sub":   {Path: "sub", IsDir: true},
	}
	lifted := addScopePrefix(rem, "photos/2024")
	r, ok := lifted["photos/2024/a.jpg"]
	if !ok || r.Path != "photos/2024/a.jpg" || r.ETag != "e1" {
		t.Errorf("a.jpg not lifted correctly: %+v ok=%v", r, ok)
	}
	if _, ok := lifted["photos/2024/sub"]; !ok {
		t.Errorf("sub not lifted")
	}
}
