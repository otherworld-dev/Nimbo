package syncguard

// Package syncguard judges whether a sync pass looks like server-side data
// loss — a server restored from an old snapshot, a shared folder someone
// emptied, or ransomware — rather than intentional change.
//
// Pure functions over a plan and a listing, free of the config and agent
// layers, so the thresholds stay exhaustively testable. The wiring (freeze
// persistence, the pass refusal, the review UI) lives in internal/agent.

import (
	"fmt"

	"github.com/otherworld/nimbo/internal/engine"
)

// sampleLimit bounds how many affected paths a trip records for the review UI.
// Enough to recognise a pattern, small enough to sit in a config file.
const sampleLimit = 20

// DefaultFloor and DefaultPct are the trip thresholds: a pass must delete or
// replace at least DefaultFloor files AND at least DefaultPct percent of the
// folder's known files. The same shape (and the same numbers) as the guard
// that protects the server from a vanished local folder — one concept, not
// two. The floor stops a four-file folder freezing when two files change; the
// percentage stops a big folder freezing over routine churn.
const (
	DefaultFloor = 50
	DefaultPct   = 50
)

// Counts summarises how destructive a plan is for the local copy.
type Counts struct {
	// Deletions are files the server no longer has.
	Deletions int
	// Overwrites are downloads landing on a path that already has a baseline
	// row — an existing file being replaced. Counted because server-side
	// ransomware REWRITES files rather than deleting them: it produces a plan
	// that is all downloads, which a deletions-only guard would wave through.
	Overwrites int
	// Sample is up to sampleLimit affected paths, for the review UI.
	Sample []string
}

// Total is the number of existing files the pass would destroy or replace.
func (c Counts) Total() int { return c.Deletions + c.Overwrites }

// Count summarises a plan. Downloads of paths with NO baseline row are
// additions, not overwrites, and are deliberately not counted — a legitimate
// bulk import is all additions and must never trip the guard.
func Count(actions []engine.Action, base map[string]engine.BaselineState) Counts {
	var c Counts
	for _, a := range actions {
		switch a.Kind {
		case engine.ActDeleteLocal:
			c.Deletions++
		case engine.ActDownload:
			if _, tracked := base[a.Path]; !tracked {
				continue // a new file, not a replacement
			}
			c.Overwrites++
		default:
			continue
		}
		if len(c.Sample) < sampleLimit {
			c.Sample = append(c.Sample, a.Path)
		}
	}
	return c
}

// Trips reports whether a plan looks like server-side damage rather than
// intentional change, and why.
//
// Both thresholds must be cleared. The floor stops a four-file folder freezing
// the moment two files change; the percentage stops a big folder freezing over
// routine churn. Same shape as the existing server-side guard in internal/agent,
// so there is one concept in the product rather than two.
func Trips(c Counts, known, floor, pct int) (reason string, trips bool) {
	total := c.Total()
	if known <= 0 || total < floor {
		return "", false
	}
	if total*100 < known*pct {
		return "", false
	}
	return fmt.Sprintf(
		"this run would remove %d and replace %d of %d known file(s) (%d%%)",
		c.Deletions, c.Overwrites, known, total*100/known), true
}

// ScanTrips reports whether a remote listing itself looks wrong, before any
// plan exists. This is the check that catches a server folder that was deleted,
// emptied, or restored from an old snapshot.
//
// It matters that this runs on the SCAN rather than the plan: propFind accepts
// a 207 with zero child responses as a perfectly valid empty listing, and
// SyncPaths builds its remote map from individual Stats without ever calling
// RemoteScan.
func ScanTrips(remoteEntries, known, pct int) (reason string, trips bool) {
	if known <= 0 {
		return "", false // nothing known yet — nothing to compare against
	}
	if remoteEntries == 0 {
		return fmt.Sprintf(
			"the server returned an empty listing for a folder with %d known file(s)", known), true
	}
	if remoteEntries*100 < known*(100-pct) {
		return fmt.Sprintf(
			"the server listing shrank from %d to %d file(s)", known, remoteEntries), true
	}
	return "", false
}

// GuardApplies reports whether the damage guard should judge this pass.
//
// A pair that has never completed a pass legitimately changes 100% of its
// files — the first run downloads everything — so guarding it would freeze
// every folder on creation. cloneStatus is state.Store.CloneStatus; only
// "done" means a pass has settled.
//
// exemptNext is the single-pass reprieve granted when the user reviews a freeze
// and chooses to resume, so the very run they approved is not refused again.
func GuardApplies(cloneStatus string, exemptNext bool) bool {
	return cloneStatus == "done" && !exemptNext
}
