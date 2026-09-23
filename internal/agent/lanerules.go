package agent

import (
	"os"
	"path/filepath"

	"github.com/otherworld/nimbo/internal/engine"
)

// gateOnLane holds a pass's actions against what the lane is doing for this
// pair (Deck #702). A path in the lane is being transferred by it: the pass
// must neither start a second transfer of it nor move or delete anything
// under a transfer that is still running.
//
//   - An upload, download or conflict of a path the lane holds is left to the
//     lane: dropped from the plan and returned in skipped, for the pass to
//     count unfinished. The lane's completion nudge re-checks the file.
//   - A move or delete of a lane path, or of a folder above one, keeps its
//     place in the plan, and the path is returned in stop: the caller stops
//     those lane transfers first. Otherwise an upload assembles at the old
//     path after the move and brings the file back, or a download holds its
//     part-file open in a folder being deleted.
//
// holds reports a lane job for exactly rel; covers, one for rel or beneath it.
func gateOnLane(actions []engine.Action, holds, covers func(rel string) bool) (keep []engine.Action, skipped, stop []string) {
	keep = make([]engine.Action, 0, len(actions))
	for _, a := range actions {
		switch a.Kind {
		case engine.ActUpload, engine.ActDownload, engine.ActConflict:
			if holds(a.Path) {
				skipped = append(skipped, a.Path)
				continue
			}
		case engine.ActMoveLocal, engine.ActMoveRemote:
			if covers(a.Path) {
				stop = append(stop, a.Path)
			}
			if a.Dest != "" && covers(a.Dest) {
				stop = append(stop, a.Dest)
			}
		case engine.ActDeleteLocal, engine.ActDeleteRemote:
			if covers(a.Path) {
				stop = append(stop, a.Path)
			}
		}
		keep = append(keep, a)
	}
	return keep, skipped, stop
}

// splitForLane takes the uploads and downloads of laneMinBytes or more out of
// a pass's actions. sizeOf returning 0 (unknown) keeps a transfer in the pass.
func splitForLane(actions []engine.Action, sizeOf func(engine.Action) int64) (pass, big []engine.Action) {
	for _, a := range actions {
		if (a.Kind == engine.ActUpload || a.Kind == engine.ActDownload) && sizeOf(a) >= laneMinBytes {
			big = append(big, a)
			continue
		}
		pass = append(pass, a)
	}
	return pass, big
}

// transferSize is how many bytes a planned transfer moves: the server's size
// for a download, the local file's for an upload. 0 when unknown or not a
// transfer.
func transferSize(p Pair, a engine.Action, remote map[string]engine.RemoteState) int64 {
	switch a.Kind {
	case engine.ActDownload:
		return remote[a.Path].Size
	case engine.ActUpload:
		if fi, err := os.Stat(filepath.Join(p.LocalDir, filepath.FromSlash(a.Path))); err == nil {
			return fi.Size()
		}
	}
	return 0
}
