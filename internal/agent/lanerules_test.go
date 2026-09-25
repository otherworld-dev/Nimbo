package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

func TestGateOnLane(t *testing.T) {
	inLane := "D/big.bin"
	holds := func(rel string) bool { return rel == inLane }
	covers := func(rel string) bool { return relUnder(inLane, rel) }
	actions := []engine.Action{
		{Kind: engine.ActUpload, Path: "D/big.bin"},                    // skip: the lane is sending it
		{Kind: engine.ActDownload, Path: "D/big.bin"},                  // skip
		{Kind: engine.ActConflict, Path: "D/big.bin"},                  // skip until the lane is done
		{Kind: engine.ActUpload, Path: "D/small.txt"},                  // unrelated: keep
		{Kind: engine.ActMoveRemote, Path: "D", Dest: "E"},             // folder above it moves: stop D, keep
		{Kind: engine.ActMoveLocal, Path: "X/a.bin", Dest: "D/big.bin"}, // lands on it: stop D/big.bin, keep
		{Kind: engine.ActDeleteLocal, Path: "D"},                       // folder above it deleted: stop D, keep
		{Kind: engine.ActDeleteRemote, Path: "DD"},                     // a different folder: keep, no stop
		{Kind: engine.ActCreateLocalDir, Path: "D/sub"},                // keep
	}
	keep, skipped, stop := gateOnLane(actions, holds, covers)

	if want := []string{"D/big.bin", "D/big.bin", "D/big.bin"}; !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
	if want := []string{"D", "D/big.bin", "D"}; !reflect.DeepEqual(stop, want) {
		t.Errorf("stop = %v, want %v", stop, want)
	}
	if want := actions[3:]; !reflect.DeepEqual(keep, want) {
		t.Errorf("keep = %v, want %v", keep, want)
	}
}

func TestSplitForLane(t *testing.T) {
	old := laneMinBytes
	laneMinBytes = 100
	t.Cleanup(func() { laneMinBytes = old })
	sizes := map[string]int64{"at.bin": 100, "below.bin": 99, "big.bin": 5000, "unknown.bin": 0}
	actions := []engine.Action{
		{Kind: engine.ActUpload, Path: "at.bin"},
		{Kind: engine.ActDownload, Path: "below.bin"},
		{Kind: engine.ActDownload, Path: "big.bin"},
		{Kind: engine.ActUpload, Path: "unknown.bin"},
		{Kind: engine.ActConflict, Path: "big.bin"}, // not a plain transfer: never the lane's
	}
	pass, big := splitForLane(actions, func(a engine.Action) int64 { return sizes[a.Path] })
	if want := []engine.Action{actions[0], actions[2]}; !reflect.DeepEqual(big, want) {
		t.Errorf("big = %v, want %v", big, want)
	}
	if want := []engine.Action{actions[1], actions[3], actions[4]}; !reflect.DeepEqual(pass, want) {
		t.Errorf("pass = %v, want %v", pass, want)
	}
}

func TestTransferSize(t *testing.T) {
	p := Pair{LocalDir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(p.LocalDir, "up.bin"), make([]byte, 1234), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := map[string]engine.RemoteState{"down.bin": {Size: 5678}}
	for _, c := range []struct {
		a    engine.Action
		want int64
	}{
		{engine.Action{Kind: engine.ActUpload, Path: "up.bin"}, 1234},     // the local file's size
		{engine.Action{Kind: engine.ActDownload, Path: "down.bin"}, 5678}, // the server's size
		{engine.Action{Kind: engine.ActUpload, Path: "gone.bin"}, 0},      // unreadable: stays in the pass
		{engine.Action{Kind: engine.ActDeleteRemote, Path: "up.bin"}, 0},
	} {
		if got := transferSize(p, c.a, remote); got != c.want {
			t.Errorf("transferSize(%v) = %d, want %d", c.a, got, c.want)
		}
	}
}
