package agent

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

func openCPTestStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct1", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCPBlobRoundtrip(t *testing.T) {
	children := []transport.Entry{
		{Path: "Work/dir_a/sub", IsDir: true, ETag: "e1", FileID: "10", Permissions: "RGDNVCK"},
		{Path: "Work/dir_a/100%_report.txt", ETag: "e2", FileID: "11", Size: 42,
			Checksums: "SHA1:aa MD5:bb", Permissions: "RGDNVW"},
		{Path: "Work/dir_a/vault", IsDir: true, ETag: "e3", IsEncrypted: true, Permissions: "RGDNVCK"},
	}
	blob, err := encodeCPBlob(children)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeCPBlob("Work/dir_a", blob)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, children) {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, children)
	}
	// Empty listing survives too.
	blob, err = encodeCPBlob(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeCPBlob("x", blob); err != nil || len(got) != 0 {
		t.Fatalf("empty roundtrip: %v %v", got, err)
	}
}

func TestScanCheckpointHitMiss(t *testing.T) {
	st := openCPTestStore(t)
	cp := newScanCheckpoint(st, "pk")
	children := []transport.Entry{{Path: "a/f", ETag: "ef", Size: 1, Permissions: "RGDNVW"}}
	cp.Save("a", "ea", children)
	got, ok := cp.Load("a", "ea")
	if !ok || !reflect.DeepEqual(got, children) {
		t.Fatalf("hit = (%v, %v), want stored children", got, ok)
	}
	if _, ok := cp.Load("a", "other"); ok {
		t.Fatal("etag mismatch must miss")
	}
	if _, ok := cp.Load("nope", "ea"); ok {
		t.Fatal("absent dir must miss")
	}
	hits, misses, saves := cp.stats()
	if hits != 1 || misses != 2 || saves != 1 {
		t.Fatalf("stats = %d/%d/%d, want 1/2/1", hits, misses, saves)
	}
}

func TestScanCheckpointEmptyETagNoops(t *testing.T) {
	st := openCPTestStore(t)
	cp := newScanCheckpoint(st, "pk")
	cp.Save("a", "", []transport.Entry{{Path: "a/f"}})
	if _, _, ok, err := st.LoadScanDir("pk", "a", ""); ok || err != nil {
		t.Fatal("empty-etag Save must not write a row")
	}
	if _, ok := cp.Load("a", ""); ok {
		t.Fatal("empty-etag Load must miss")
	}
	if _, misses, saves := cp.stats(); misses != 0 || saves != 0 {
		t.Fatal("no-op guards must not count as activity")
	}
}

func TestScanCheckpointUnknownFormatMisses(t *testing.T) {
	st := openCPTestStore(t)
	blob, err := encodeCPBlob(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveScanDir("pk", "a", "ea", 99, blob); err != nil {
		t.Fatal(err)
	}
	cp := newScanCheckpoint(st, "pk")
	if _, ok := cp.Load("a", "ea"); ok {
		t.Fatal("unknown fmt must read as a miss")
	}
}

func TestScanCheckpointCorruptBlobMisses(t *testing.T) {
	st := openCPTestStore(t)
	if err := st.SaveScanDir("pk", "a", "ea", cpFormat, []byte("not gzip")); err != nil {
		t.Fatal(err)
	}
	cp := newScanCheckpoint(st, "pk")
	if _, ok := cp.Load("a", "ea"); ok {
		t.Fatal("corrupt blob must read as a miss")
	}
	if cp.tripped() {
		t.Fatal("corrupt row is data, not a store failure — must not trip the fuse")
	}
}

func TestScanCheckpointStickyFuse(t *testing.T) {
	st := openCPTestStore(t)
	st.Close() // every store call now errors
	cp := newScanCheckpoint(st, "pk")
	cp.Save("a", "ea", nil) // must not panic; trips the fuse
	if !cp.tripped() {
		t.Fatal("store error must trip the fuse")
	}
	if _, ok := cp.Load("a", "ea"); ok {
		t.Fatal("tripped handle must miss")
	}
	if _, _, saves := cp.stats(); saves != 0 {
		t.Fatal("failed save counted")
	}
}

func TestClearCheckpointGuard(t *testing.T) {
	st := openCPTestStore(t)
	e := &Engine{}
	seed := func() {
		if err := st.SaveScanDir("pk", "dir", "e", cpFormat, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	rowPresent := func() bool {
		_, _, ok, err := st.LoadScanDir("pk", "dir", "e")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// Unknown pair = assume dirty (a previous process life may have left rows).
	seed()
	e.clearCheckpoint(st, "pk")
	if rowPresent() {
		t.Fatal("first clear did not delete")
	}
	// Marked clean now: the guard suppresses the redundant DELETE.
	seed()
	e.clearCheckpoint(st, "pk")
	if !rowPresent() {
		t.Fatal("guard failed: clean pair was re-cleared (want the DELETE skipped)")
	}
	// A scan that saved rows re-dirties; the next clear deletes again.
	e.markCheckpointDirty("pk")
	e.clearCheckpoint(st, "pk")
	if rowPresent() {
		t.Fatal("clear after re-dirty did not delete")
	}
}
