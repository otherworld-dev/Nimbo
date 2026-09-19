package transfer

import "testing"

// The capacity lookup must read something real for an ordinary fixed drive,
// or every mirrored deletion would be moved aside.
func TestVolumeBinCapacityReadsTheDrive(t *testing.T) {
	capacity, ok := volumeBinCapacity(t.TempDir())
	if !ok {
		t.Fatal("Windows has Recycle Bins; ok must be true")
	}
	t.Logf("Recycle Bin capacity on the temp drive: %d MB", capacity>>20)
	if capacity <= 0 {
		t.Skip("the Recycle Bin is off on this drive")
	}
}
