package engine

import (
	"reflect"
	"testing"
)

// A folder shared with you that is unshared — or a group folder you were
// removed from, or an external storage the admin unmounted — simply vanishes
// from the WebDAV tree. The diff cannot tell that from the owner deleting its
// contents, so it plans a local delete of the whole subtree. Below the damage
// guard's thresholds that used to be permanent (Deck #557).
//
// The one thing that distinguishes the two is the baseline: the vanished
// directory was the ROOT of a share or mount. KeepDetached uses that to leave
// the local copy alone.
func TestKeepDetachedKeepsAnUnsharedFolder(t *testing.T) {
	base := map[string]BaselineState{
		"Team":             {Path: "Team", IsDir: true, MountRoot: true},
		"Team/Budget.xlsx": {Path: "Team/Budget.xlsx"},
		"Team/sub":         {Path: "Team/sub", IsDir: true},
		"Team/sub/a.txt":   {Path: "Team/sub/a.txt"},
		"Team/edited.txt":  {Path: "Team/edited.txt"},
		"mine.txt":         {Path: "mine.txt"},
		"gone.txt":         {Path: "gone.txt"},
	}
	actions := []Action{
		{Kind: ActDeleteLocal, Path: "Team"},
		{Kind: ActDeleteLocal, Path: "Team/Budget.xlsx"},
		{Kind: ActDeleteLocal, Path: "Team/sub"},
		{Kind: ActDeleteLocal, Path: "Team/sub/a.txt"},
		// A file the user edited after the share went: the diff calls it a
		// conflict. Resolving it would resurrect it on the server, which is not
		// what happened — the whole subtree is left alone this pass.
		{Kind: ActConflict, Path: "Team/edited.txt"},
		// A brand-new local file under the share, never uploaded. Uploading it
		// now would fail (its parent is gone) — dropped with the rest.
		{Kind: ActUpload, Path: "Team/new.txt"},
		// Unrelated work must be untouched.
		{Kind: ActUpload, Path: "mine.txt"},
		{Kind: ActDeleteLocal, Path: "gone.txt"}, // an ordinary server-side delete
	}

	out, detached := KeepDetached(actions, base)

	if !reflect.DeepEqual(detached, []string{"Team"}) {
		t.Fatalf("detached = %v, want [Team]", detached)
	}
	want := []Action{
		{Kind: ActUpload, Path: "mine.txt"},
		{Kind: ActDeleteLocal, Path: "gone.txt"},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("plan = %+v\nwant %+v", out, want)
	}
}

// A folder that only LOOKS like a prefix of the share root is not under it:
// "Teams" is not inside "Team".
func TestKeepDetachedMatchesWholePathSegments(t *testing.T) {
	base := map[string]BaselineState{
		"Team":  {Path: "Team", IsDir: true, MountRoot: true},
		"Teams": {Path: "Teams", IsDir: true},
	}
	actions := []Action{
		{Kind: ActDeleteLocal, Path: "Team"},
		{Kind: ActDeleteLocal, Path: "Teams"},
	}
	out, _ := KeepDetached(actions, base)
	if len(out) != 1 || out[0].Path != "Teams" {
		t.Errorf("sibling with a shared prefix was swallowed: %+v", out)
	}
}

// A share root nested under the user's own folder is still a share root, and
// a subfolder INSIDE a share (no MountRoot flag) is an ordinary deletion.
func TestKeepDetachedHandlesNestedRoots(t *testing.T) {
	base := map[string]BaselineState{
		"Projects":          {Path: "Projects", IsDir: true},
		"Projects/Team":     {Path: "Projects/Team", IsDir: true, MountRoot: true},
		"Projects/Team/x":   {Path: "Projects/Team/x"},
		"Projects/Team/old": {Path: "Projects/Team/old", IsDir: true}, // a subfolder of the share
	}
	// Only the subfolder went: the owner deleted it. Mirror that.
	out, detached := KeepDetached([]Action{{Kind: ActDeleteLocal, Path: "Projects/Team/old"}}, base)
	if len(detached) != 0 || len(out) != 1 {
		t.Errorf("a subfolder of a share must delete normally: out=%+v detached=%v", out, detached)
	}
	// The whole share went while its parent stayed: an unshare.
	out, detached = KeepDetached([]Action{
		{Kind: ActDeleteLocal, Path: "Projects/Team"},
		{Kind: ActDeleteLocal, Path: "Projects/Team/x"},
	}, base)
	if !reflect.DeepEqual(detached, []string{"Projects/Team"}) || len(out) != 0 {
		t.Errorf("nested share root not kept: out=%+v detached=%v", out, detached)
	}
}

// If the share root's PARENT is being deleted too, the user deleted the parent
// folder (on another device or the web), taking the share mount with it. That
// is a real deletion to mirror: keeping the share would leave the parent
// behind and re-upload it next pass, undoing what the user did.
func TestKeepDetachedFollowsADeletedParent(t *testing.T) {
	base := map[string]BaselineState{
		"Projects":        {Path: "Projects", IsDir: true},
		"Projects/Team":   {Path: "Projects/Team", IsDir: true, MountRoot: true},
		"Projects/Team/x": {Path: "Projects/Team/x"},
	}
	actions := []Action{
		{Kind: ActDeleteLocal, Path: "Projects"},
		{Kind: ActDeleteLocal, Path: "Projects/Team"},
		{Kind: ActDeleteLocal, Path: "Projects/Team/x"},
	}
	out, detached := KeepDetached(actions, base)
	if len(detached) != 0 {
		t.Errorf("detached = %v, want none", detached)
	}
	if !reflect.DeepEqual(out, actions) {
		t.Errorf("plan altered: %+v", out)
	}
}

// A single FILE shared with you is a mount root too, and it vanishes the same
// way when unshared.
func TestKeepDetachedKeepsAnUnsharedFile(t *testing.T) {
	base := map[string]BaselineState{
		"Budget.xlsx": {Path: "Budget.xlsx", MountRoot: true},
	}
	out, detached := KeepDetached([]Action{{Kind: ActDeleteLocal, Path: "Budget.xlsx"}}, base)
	if !reflect.DeepEqual(detached, []string{"Budget.xlsx"}) || len(out) != 0 {
		t.Errorf("shared file not kept: out=%+v detached=%v", out, detached)
	}
}

// Only a local delete of a KNOWN mount root qualifies. A path with no baseline
// row (or a flag-less one), and any other action kind, pass straight through.
func TestKeepDetachedLeavesEverythingElseAlone(t *testing.T) {
	base := map[string]BaselineState{
		"plain":  {Path: "plain", IsDir: true},
		"shared": {Path: "shared", IsDir: true, MountRoot: true},
	}
	actions := []Action{
		{Kind: ActDeleteLocal, Path: "plain"},
		{Kind: ActDeleteLocal, Path: "unknown"},
		{Kind: ActDeleteRemote, Path: "shared"}, // removed LOCALLY by the user: propagate as ever
		{Kind: ActDownload, Path: "shared/new.txt"},
	}
	out, detached := KeepDetached(actions, base)
	if len(detached) != 0 {
		t.Errorf("detached = %v, want none", detached)
	}
	if !reflect.DeepEqual(out, actions) {
		t.Errorf("plan altered: %+v", out)
	}
	if out, detached := KeepDetached(nil, base); len(out) != 0 || len(detached) != 0 {
		t.Errorf("empty plan: out=%v detached=%v", out, detached)
	}
}

// Two shares going in one pass are reported in a stable order.
func TestKeepDetachedReportsRootsSorted(t *testing.T) {
	base := map[string]BaselineState{
		"b": {Path: "b", IsDir: true, MountRoot: true},
		"a": {Path: "a", IsDir: true, MountRoot: true},
	}
	_, detached := KeepDetached([]Action{
		{Kind: ActDeleteLocal, Path: "b"},
		{Kind: ActDeleteLocal, Path: "a"},
	}, base)
	if !reflect.DeepEqual(detached, []string{"a", "b"}) {
		t.Errorf("detached = %v, want [a b]", detached)
	}
}
