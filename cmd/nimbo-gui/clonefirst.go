package main

import (
	"encoding/json"
	"strings"

	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/config"
)

// Download these first (Deck #766): folders the first sync downloads before
// everything else. Chosen in the setup wizard (CompleteSetup's "everything"
// mode carries them) or while a first sync runs (Status → First sync, through
// SetSyncMode's "clone-first:" sub-command). Both ride on existing string
// arguments, so no new App method and no bindings regen for the calls; the
// running first syncs reach the UI as ProgressDTO.Clones.

// CloneRunDTO is a first sync in progress, for the Status window.
type CloneRunDTO struct {
	LocalDir   string   `json:"localDir"`
	RemoteRoot string   `json:"remoteRoot"`
	First      []string `json:"first"` // pair-relative, in order
}

func toCloneRunDTOs(runs []agent.CloneRun) []CloneRunDTO {
	out := make([]CloneRunDTO, 0, len(runs))
	for _, r := range runs {
		first := r.First
		if first == nil {
			first = []string{}
		}
		out = append(out, CloneRunDTO{LocalDir: r.LocalDir, RemoteRoot: r.RemoteRoot, First: first})
	}
	return out
}

// setupFirstPrefix starts CompleteSetup's mode when the user picked folders to
// download first: "everything:" followed by a JSON array of account paths.
const setupFirstPrefix = "everything:"

// parseSetupMode splits CompleteSetup's mode into the mode and any folders to
// download first. A malformed list is ignored rather than failing the setup:
// it only changes the order.
func parseSetupMode(mode string) (string, []string) {
	if !strings.HasPrefix(mode, setupFirstPrefix) {
		return mode, nil
	}
	var first []string
	_ = json.Unmarshal([]byte(strings.TrimPrefix(mode, setupFirstPrefix)), &first)
	return "everything", first
}

// cloneFirstPrefix is SetSyncMode's sub-command for a running first sync:
// "clone-first:" followed by {"localDir": ..., "paths": [...]}, the paths
// relative to that sync folder's remote root, in order ([] = normal order).
const cloneFirstPrefix = "clone-first:"

// setCloneFirst applies a clone-first sub-command to whichever engine is
// running that folder's first sync. Returns "" or a message for the UI.
func (a *App) setCloneFirst(arg string) string {
	var req struct {
		LocalDir string   `json:"localDir"`
		Paths    []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(arg), &req); err != nil || req.LocalDir == "" {
		return "couldn't read that choice"
	}
	msg := "That folder's first sync has already finished."
	a.firstEngine(func(e *agent.Engine) bool {
		for _, r := range e.CloneRuns() {
			if config.PathKey(r.LocalDir) != config.PathKey(req.LocalDir) {
				continue
			}
			if err := e.SetCloneFirst(r.LocalDir, r.RemoteRoot, req.Paths); err != nil {
				msg = err.Error()
			} else {
				msg = ""
			}
			return true
		}
		return false
	})
	return msg
}
