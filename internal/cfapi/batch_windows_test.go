package cfapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A placeholder batch is poisoned by one long identity.
//
// Measured on Windows 11 10.0.26200 (2026-09-15/16, on the test VM and here):
// when CfCreatePlaceholders or CfExecute(TRANSFER_PLACEHOLDERS) is handed an
// array in which ONE entry carries a FileIdentity of ~133 bytes or more, that
// entry is created correctly and EVERY OTHER entry of the same call comes out
// with corrupt cloud-file metadata — ERROR_CLOUD_FILE_METADATA_CORRUPT (363)
// on any open, even as the connected provider, undeletable, permanent. 132
// bytes is fine; 136 is not; the name length is irrelevant. An identity that
// long, created alone, is healthy.
//
// Nimbo's identity is the raw server path, so any directory holding an entry
// with a path that long stranded its siblings on population (the shell's
// FETCH_PLACEHOLDERS) and on reconcile's pull alike. GitHub issue #7's
// "corrupt metadata" placeholders were this.
//
// The provider's defence is to never put a long identity in the same batch
// as anything else; this test pins the fault and the defence on both paths.
//
// RUN THESE ONLY AGAINST A TREE THAT HAS THE DEFENCE. On an unfixed tree they
// reproduce the fault for real: the poisoned placeholders they leave in the
// test's temp directory cannot be opened, repaired or deleted by any normal
// means (t.TempDir's cleanup fails too), so they sit under %TEMP% until
// somebody runs the fltmc detach recipe from docs/TROUBLESHOOTING.md.

const batchProbeShort = 10

func batchHealthy(t *testing.T, path string) bool {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Logf("%s: Lstat: %v", filepath.Base(path), err)
		return false
	}
	if _, err := PlaceholderIdentity(path); err != nil {
		t.Logf("%s: PlaceholderIdentity: %v", filepath.Base(path), err)
		return false
	}
	return true
}

func batchMount(t *testing.T, list ListFunc) (string, int64) {
	t.Helper()
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "batchroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	connKey, err := Mount(root, "NimboBatchTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { Unmount(root, connKey) })
	return root, connKey
}

func batchItems(prefix string) []PlaceholderInfo {
	long := strings.Repeat("p", 220) // a 220-byte identity: well past the fault's threshold
	return []PlaceholderInfo{
		{Name: prefix + "a.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/" + prefix + "a.bin")},
		{Name: prefix + "deep.bin", Size: 4096, ModTime: time.Now(), Identity: []byte(long)},
		{Name: prefix + "b.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/" + prefix + "b.bin")},
		{Name: prefix + "d", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/" + prefix + "d")},
	}
}

func TestCreatePlaceholdersSurvivesALongIdentityInTheBatch(t *testing.T) {
	root, _ := batchMount(t, func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} })
	items := batchItems("c")
	if err := CreatePlaceholders(root, items); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	for _, it := range items {
		if !batchHealthy(t, filepath.Join(root, it.Name)) {
			t.Errorf("%s: not a readable placeholder after a batch that carried a %d-byte identity", it.Name, len(items[1].Identity))
		}
	}
	if id, err := PlaceholderIdentity(filepath.Join(root, "cdeep.bin")); err != nil || string(id) != string(items[1].Identity) {
		t.Errorf("long identity: %q, %v", id, err)
	}
}

// transferProbe watches the transfer leg of a live mount.
//
// Mount seeds the root itself — list("") straight into CreatePlaceholders,
// before it returns — so a listing that is ready from the start is delivered
// by the SEED and the transfer never sees an entry. That is how the first
// version of this test passed while proving nothing: all four entries existed
// by the time anything enumerated the root, the Lstat pre-filter dropped them
// all, and the transfer carried zero.
//
// So the listing answers "unknown" (nil — the provider-cannot-reach-the-server
// answer, which leaves the root unpopulated and the shell free to ask again)
// until ready is set. The shell re-asks the root after every connect, so the
// entries then arrive through a real FETCH_PLACEHOLDERS.
type transferProbe struct {
	ready      atomic.Bool
	fetches    atomic.Int32 // root listings answered after ready
	mu         sync.Mutex
	transfers  []int    // entries carried by each TRANSFER_PLACEHOLDERS after ready
	complaints []string // long-alone / incomplete debug lines after ready
}

// complained returns the transfer's own complaints since ready: a long-identity
// entry it failed to create alone, or a delivery it called incomplete. Both are
// silent in production (the return value is discarded), and an entry that
// ALREADY EXISTS produces exactly these if it is not filtered out of the batch
// first — which is what makes them the pre-filter's teeth.
func (p *transferProbe) complained() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.complaints...)
}

// carried returns the largest number of entries any post-ready transfer
// delivered (0 if none ran).
func (p *transferProbe) carried() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	max := 0
	for _, n := range p.transfers {
		if n > max {
			max = n
		}
	}
	return max
}

func (p *transferProbe) counts() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.transfers...)
}

func transferProbeMount(t *testing.T, items []PlaceholderInfo) (string, *transferProbe) {
	t.Helper()
	p := &transferProbe{}
	root, _ := batchMount(t, func(rel string) []PlaceholderInfo {
		if rel != "" {
			return []PlaceholderInfo{}
		}
		if !p.ready.Load() {
			return nil
		}
		p.fetches.Add(1)
		return items
	})
	old := Debug
	Debug = func(format string, args ...any) {
		if !p.ready.Load() {
			return
		}
		switch {
		case strings.Contains(format, "CfExecute(TRANSFER_PLACEHOLDERS) count="):
			if len(args) == 0 {
				return
			}
			if n, ok := args[0].(int); ok {
				p.mu.Lock()
				p.transfers = append(p.transfers, n)
				p.mu.Unlock()
			}
		case strings.Contains(format, "created alone ->"), strings.Contains(format, "transfer incomplete"):
			p.mu.Lock()
			p.complaints = append(p.complaints, fmt.Sprintf(format, args...))
			p.mu.Unlock()
		}
	}
	t.Cleanup(func() { Debug = old })
	return root, p
}

// awaitTransfer enumerates the root until want exists, then gives any further
// fetch a moment to land. It never creates anything itself: if the shell does
// not re-ask the root, the test has nothing to say and must fail.
func (p *transferProbe) awaitTransfer(t *testing.T, root, want string) {
	t.Helper()
	p.ready.Store(true)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = os.ReadDir(root)
		if _, err := os.Lstat(filepath.Join(root, want)); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Second)
	if p.fetches.Load() == 0 {
		t.Fatalf("the root was never re-fetched after the mount (post-mount root listings: %d) — the transfer leg was never exercised", p.fetches.Load())
	}
}

func TestTransferPlaceholdersSurvivesALongIdentityInTheBatch(t *testing.T) {
	items := batchItems("t")
	root, probe := transferProbeMount(t, items)

	probe.awaitTransfer(t, root, "tb.bin")

	// The three short entries travel in the transfer; the long one is created
	// alone beforehand. A transfer carrying all four is the poisoning shape.
	if got, want := probe.carried(), len(items)-1; got != want {
		t.Errorf("the biggest transfer carried %d entries, want %d (counts: %v)", got, want, probe.counts())
	}
	if c := probe.complained(); len(c) != 0 {
		t.Errorf("the transfer complained: %v — the long entry must be created alone and succeed", c)
	}
	for _, it := range items {
		if !batchHealthy(t, filepath.Join(root, it.Name)) {
			t.Errorf("%s: not a readable placeholder after a transfer whose listing carried a %d-byte identity", it.Name, len(items[1].Identity))
		}
	}
	if id, err := PlaceholderIdentity(filepath.Join(root, "tdeep.bin")); err != nil || string(id) != string(items[1].Identity) {
		t.Errorf("long identity: %q, %v", id, err)
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != len(items) {
		t.Errorf("root lists %d entries, want %d", len(ents), len(items))
	}
}

// The reporter's own shape: the long-identity entry ALREADY EXISTS locally
// (created alone, healthy) and the listing that answers the shell's re-fetch
// carries it along with the three others. The kernel would answer
// ALREADY_EXISTS for it — harmless in itself — but an existing entry with a
// long identity poisons its batch-mates exactly like a new one, so the Lstat
// pre-filter has to drop it before the batch is built.
func TestTransferPlaceholdersSkipsALongIdentityThatAlreadyExists(t *testing.T) {
	items := batchItems("x")
	root, probe := transferProbeMount(t, items)
	long := items[1]
	if err := CreatePlaceholders(root, []PlaceholderInfo{long}); err != nil {
		t.Fatalf("CreatePlaceholders(long alone): %v", err)
	}
	if !batchHealthy(t, filepath.Join(root, long.Name)) {
		t.Fatalf("%s: the long entry was not healthy before the transfer", long.Name)
	}

	probe.awaitTransfer(t, root, "xb.bin")

	if got, want := probe.carried(), len(items)-1; got != want {
		t.Errorf("the biggest transfer carried %d entries, want %d — the pre-existing long entry must be filtered out (counts: %v)", got, want, probe.counts())
	}
	// The teeth of the Lstat pre-filter: without it the long entry, which is
	// already there, is split out and created ALONE, that create fails with
	// ALREADY_EXISTS, and the transfer calls itself incomplete. Both lines are
	// silent in production, so the test is the only thing that can see them.
	if c := probe.complained(); len(c) != 0 {
		t.Errorf("the transfer complained: %v — an entry that already exists must be dropped before the batch is built", c)
	}
	for _, it := range items {
		if !batchHealthy(t, filepath.Join(root, it.Name)) {
			t.Errorf("%s: not a readable placeholder after a transfer alongside a pre-existing %d-byte identity", it.Name, len(long.Identity))
		}
	}
	if id, err := PlaceholderIdentity(filepath.Join(root, long.Name)); err != nil || string(id) != string(long.Identity) {
		t.Errorf("the pre-existing long entry changed: identity %q, %v", id, err)
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != len(items) {
		t.Errorf("root lists %d entries, want %d", len(ents), len(items))
	}
}
