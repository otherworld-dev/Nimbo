package main

import (
	"reflect"
	"testing"

	"github.com/otherworld/nimbo/internal/agent"
)

func TestParseSetupMode(t *testing.T) {
	for _, tc := range []struct {
		in        string
		mode      string
		wantFirst []string
	}{
		{"everything", "everything", nil},
		{"choose", "choose", nil},
		{"ondemand", "ondemand", nil},
		{`everything:["Work/Current","Photos"]`, "everything", []string{"Work/Current", "Photos"}},
		{"everything:[]", "everything", []string{}},
		{"everything:not json", "everything", nil}, // only the order is lost, never the setup
	} {
		mode, first := parseSetupMode(tc.in)
		if mode != tc.mode || !reflect.DeepEqual(first, tc.wantFirst) {
			t.Errorf("parseSetupMode(%q) = %q, %v; want %q, %v", tc.in, mode, first, tc.mode, tc.wantFirst)
		}
	}
}

// The UI reads First as a list, never null.
func TestToCloneRunDTOs(t *testing.T) {
	got := toCloneRunDTOs([]agent.CloneRun{{LocalDir: `C:\Nimbo`}, {LocalDir: `D:\Work`, RemoteRoot: "Work", First: []string{"a"}}})
	want := []CloneRunDTO{{LocalDir: `C:\Nimbo`, First: []string{}}, {LocalDir: `D:\Work`, RemoteRoot: "Work", First: []string{"a"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := toCloneRunDTOs(nil); got == nil || len(got) != 0 {
		t.Fatalf("no runs = %#v, want an empty list", got)
	}
}

// A choice for a folder with no first sync running is refused with a message.
func TestSetCloneFirstNoRun(t *testing.T) {
	a := &App{}
	if msg := a.setCloneFirst(`{"localDir":"C:\\Nimbo","paths":["x"]}`); msg == "" {
		t.Fatal("a choice with no first sync running was accepted")
	}
	if msg := a.setCloneFirst(`nonsense`); msg == "" {
		t.Fatal("a malformed choice was accepted")
	}
}
