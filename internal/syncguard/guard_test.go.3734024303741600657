package syncguard

import (
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

func base(paths ...string) map[string]engine.BaselineState {
	m := make(map[string]engine.BaselineState, len(paths))
	for _, p := range paths {
		m[p] = engine.BaselineState{Path: p}
	}
	return m
}

func TestCountSeparatesDeletionsFromOverwrites(t *testing.T) {
	actions := []engine.Action{
		{Kind: engine.ActDeleteLocal, Path: "gone.txt"},
		{Kind: engine.ActDownload, Path: "tracked.txt"}, // has a baseline -> overwrite
		{Kind: engine.ActDownload, Path: "brand-new.txt"},
	}
	c := Count(actions, base("gone.txt", "tracked.txt"))
	if c.Deletions != 1 {
		t.Fatalf("want 1 deletion, got %d", c.Deletions)
	}
	if c.Overwrites != 1 {
		t.Fatalf("want 1 overwrite, got %d", c.Overwrites)
	}
}

// The whole point of the design: a big import is additions, and must not freeze.
func TestCountIgnoresNewDownloads(t *testing.T) {
	var actions []engine.Action
	for i := 0; i < 5000; i++ {
		actions = append(actions, engine.Action{Kind: engine.ActDownload, Path: string(rune(i)) + ".jpg"})
	}
	c := Count(actions, base())
	if c.Overwrites != 0 {
		t.Fatalf("downloads of untracked paths are additions, got %d overwrites", c.Overwrites)
	}
}

func TestCountCollectsASample(t *testing.T) {
	var actions []engine.Action
	for i := 0; i < 50; i++ {
		actions = append(actions, engine.Action{Kind: engine.ActDeleteLocal, Path: string(rune('a'+i%26)) + ".txt"})
	}
	c := Count(actions, base())
	if len(c.Sample) == 0 || len(c.Sample) > sampleLimit {
		t.Fatalf("want a sample of at most %d paths, got %d", sampleLimit, len(c.Sample))
	}
}

func TestTripsNeedsBothFloorAndPercentage(t *testing.T) {
	// Over the percentage but under the floor: a small folder churning is normal.
	if _, trips := Trips(Counts{Deletions: 10}, 10, 50, 50); trips {
		t.Fatal("10 files is under the floor of 50 — must not trip")
	}
	// Over the floor but a small fraction: a big folder losing a few files is normal.
	if _, trips := Trips(Counts{Deletions: 60}, 10000, 50, 50); trips {
		t.Fatal("60 of 10000 is under 50% — must not trip")
	}
	// Both: this is the damage signature.
	if _, trips := Trips(Counts{Deletions: 60}, 100, 50, 50); !trips {
		t.Fatal("60 of 100 clears both thresholds — must trip")
	}
}

func TestTripsCountsOverwritesTowardTheThreshold(t *testing.T) {
	// Nothing deleted at all — every file rewritten. This is ransomware.
	reason, trips := Trips(Counts{Overwrites: 900}, 1000, 50, 50)
	if !trips {
		t.Fatal("900 of 1000 files rewritten must trip — a deletions-only guard is the bug this exists to prevent")
	}
	if !strings.Contains(reason, "900") {
		t.Fatalf("reason should quote the count, got %q", reason)
	}
}

func TestTripsWithNoKnownFilesNeverTrips(t *testing.T) {
	if _, trips := Trips(Counts{Deletions: 1000}, 0, 50, 50); trips {
		t.Fatal("a pair with no baseline has nothing to protect — must not trip")
	}
}

func TestScanTripsOnEmptyListing(t *testing.T) {
	reason, trips := ScanTrips(0, 5000, 50)
	if !trips {
		t.Fatal("an empty remote listing against a populated baseline must trip")
	}
	if !strings.Contains(strings.ToLower(reason), "empty") {
		t.Fatalf("reason should say the listing was empty, got %q", reason)
	}
}

func TestScanTripsOnLargeShrink(t *testing.T) {
	if _, trips := ScanTrips(400, 5000, 50); !trips {
		t.Fatal("400 entries against 5000 known is a 92% shrink — must trip")
	}
	if _, trips := ScanTrips(4900, 5000, 50); trips {
		t.Fatal("4900 against 5000 is normal churn — must not trip")
	}
}

func TestScanTripsIgnoresAnEmptyBaseline(t *testing.T) {
	if _, trips := ScanTrips(0, 0, 50); trips {
		t.Fatal("no baseline means nothing to compare — must not trip")
	}
}

// Spec invariant 6: a pair that has never completed a pass legitimately changes
// 100%, so the guard must not apply to it.
func TestGuardAppliesOnlyAfterAFirstCompletedPass(t *testing.T) {
	if GuardApplies("done", false) != true {
		t.Fatal("a settled pair must be guarded")
	}
	if GuardApplies("started", false) {
		t.Fatal("a pair still cloning must not be guarded — its first pass changes everything")
	}
	if GuardApplies("", false) {
		t.Fatal("a pair with no clone status has never completed a pass — must not be guarded")
	}
	if GuardApplies("done", true) {
		t.Fatal("an approved resume grants a one-pass exemption")
	}
}

