package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// PostRevertFile records folders that have just left virtual-files mode and have
// not yet completed a live sync pass.
//
// It exists to prevent data loss. Leaving on-demand mode can leave the local
// tree incomplete — a directory whose placeholders never populated is simply
// empty on disk — and the reconciler cannot tell "never materialised" from
// "the user deleted it". On 2026-08-16 that difference deleted a shared file
// from the server, and every other machine then removed its local copy.
//
// A revert never intends to delete anything, so the first pass afterwards
// restores what is missing instead of deleting it. See Deck #571.
func (d Dirs) PostRevertFile() string {
	return filepath.Join(d.Config, "post-revert.json")
}

// LoadPostRevert returns the folders awaiting their first live pass.
func (d Dirs) LoadPostRevert() []string {
	data, err := os.ReadFile(d.PostRevertFile())
	if err != nil {
		return nil
	}
	var out []string
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// MarkPostRevert records that dir has just left virtual-files mode.
func (d Dirs) MarkPostRevert(dir string) error {
	cur := d.LoadPostRevert()
	for _, p := range cur {
		if strings.EqualFold(p, dir) {
			return nil
		}
	}
	return d.savePostRevert(append(cur, dir))
}

// ClearPostRevert drops dir once it has completed a clean pass.
func (d Dirs) ClearPostRevert(dir string) error {
	cur := d.LoadPostRevert()
	out := make([]string, 0, len(cur))
	for _, p := range cur {
		if !strings.EqualFold(p, dir) {
			out = append(out, p)
		}
	}
	if len(out) == len(cur) {
		return nil
	}
	return d.savePostRevert(out)
}

// IsPostRevert reports whether dir is still awaiting its first live pass.
func (d Dirs) IsPostRevert(dir string) bool {
	for _, p := range d.LoadPostRevert() {
		if strings.EqualFold(p, dir) {
			return true
		}
	}
	return false
}

func (d Dirs) savePostRevert(dirs []string) error {
	if dirs == nil {
		dirs = []string{}
	}
	data, err := json.MarshalIndent(dirs, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.PostRevertFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.PostRevertFile())
}
