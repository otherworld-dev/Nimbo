//go:build windows

package cfapi

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// Live-driver tests for Deck #701, opt in with NIMBO_CFAPI_LIVE=1 (see
// streamRoot).

// rawTransfer issues a CfExecute(TRANSFER_DATA) with no buffer, returning the
// raw HRESULT, so the filter's view of a zero-length completion can be pinned.
func rawTransfer(connKey, transferKey, offset, length int64, status int32) uint32 {
	oi := operationInfo{Type: cfOperationTypeTransferData, ConnectionKey: connKey, TransferKey: transferKey}
	oi.StructSize = uint32(unsafe.Sizeof(oi))
	op := opParamsTransfer{CompletionStatus: status, Offset: offset, Length: length}
	op.ParamSize = uint32(unsafe.Sizeof(op))
	hr, _, _ := procCfExecute.Call(uintptr(unsafe.Pointer(&oi)), uintptr(unsafe.Pointer(&op)))
	return uint32(hr)
}

// The measurement failFetch's choice rests on. On a live transfer key:
//   - a completion with Length 0 is REJECTED, success and failure alike
//     (0x8007017C, ERROR_CLOUD_FILE_INVALID_REQUEST), and leaves the request
//     open — the file still hydrates normally afterwards;
//   - a failure with Length = CF_EOF is ACCEPTED and fails the open at once.
//
// So a request with no length can only be answered by the second.
func TestZeroLengthCompletionIsRejectedButAnEOFFailIsNot(t *testing.T) {
	const errCloudFileInvalidRequest = 0x8007017C
	const size = 1 << 20
	content := patternBytes(size)

	t.Run("zero length", func(t *testing.T) {
		root, connKey, _ := streamRoot(t, "zerolen")
		SetHydrateStream(connKey, func(_ context.Context, _ []byte, offset, length int64) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(content[offset : offset+length])), nil
		})
		var once sync.Once
		var success, fail uint32
		old := execTransfer
		execTransfer = func(c, k, off int64, data []byte) bool {
			once.Do(func() {
				success = rawTransfer(c, k, off, 0, 0)
				fail = rawTransfer(c, k, off, 0, statusUnsuccessful)
			})
			return old(c, k, off, data)
		}
		t.Cleanup(func() { execTransfer = old })
		if err := CreatePlaceholders(root, []PlaceholderInfo{{Name: "f.bin", Size: size, ModTime: time.Now(), Identity: []byte("remote/f.bin")}}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root, "f.bin"))
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("read after the zero-length completions: %d bytes, err=%v — they disturbed the request", len(got), err)
		}
		if success != errCloudFileInvalidRequest || fail != errCloudFileInvalidRequest {
			t.Errorf("zero-length success -> 0x%08x, failure -> 0x%08x; want both rejected with 0x%08x", success, fail, uint32(errCloudFileInvalidRequest))
		}
	})

	t.Run("EOF fail", func(t *testing.T) {
		root, connKey, _ := streamRoot(t, "eoffail")
		SetHydrateStream(connKey, func(_ context.Context, _ []byte, _, length int64) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(make([]byte, length))), nil
		})
		// Answer each request with the CF_EOF failure and nothing else: the
		// stream's own failure for the refused piece is swallowed, so the open
		// can only fail promptly if the CF_EOF failure completed the request.
		var hrs []uint32
		var mu sync.Mutex
		oldT, oldF := execTransfer, execTransferFail
		execTransfer = func(c, k, off int64, _ []byte) bool {
			hr := rawTransfer(c, k, off, cfEOF, statusUnsuccessful)
			mu.Lock()
			hrs = append(hrs, hr)
			mu.Unlock()
			return false
		}
		execTransferFail = func(_, _, _, _ int64) {}
		t.Cleanup(func() { execTransfer, execTransferFail = oldT, oldF })
		if err := CreatePlaceholders(root, []PlaceholderInfo{{Name: "f.bin", Size: size, ModTime: time.Now(), Identity: []byte("remote/f.bin")}}); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := os.ReadFile(filepath.Join(root, "f.bin")); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Error("the read succeeded; want it failed by the CF_EOF failure")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the read is still blocked: the CF_EOF failure did not complete the request")
		}
		mu.Lock()
		defer mu.Unlock()
		if len(hrs) == 0 {
			t.Fatal("no request was answered")
		}
		for _, hr := range hrs {
			if hr != 0 {
				t.Errorf("CF_EOF failure -> 0x%08x, want accepted (0)", hr)
			}
		}
	})
}

// MountStream puts the stream in place before the provider connects, so it
// serves every request from the first.
func TestMountStreamServesTheFirstRequest(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "mountstream")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	const size = 3<<20 + 77
	content := patternBytes(size)
	var opens atomic.Int64
	stream := func(_ context.Context, identity []byte, offset, length int64) (io.ReadCloser, error) {
		opens.Add(1)
		return io.NopCloser(bytes.NewReader(content[offset : offset+length])), nil
	}
	connKey, err := MountStream(root, "NimboMountStream", "", stream, func(string) []PlaceholderInfo { return []PlaceholderInfo{} })
	if err != nil {
		t.Fatalf("MountStream: %v", err)
	}
	t.Cleanup(func() { Unmount(root, connKey) })

	if err := CreatePlaceholders(root, []PlaceholderInfo{{Name: "f.bin", Size: size, ModTime: time.Now(), Identity: []byte("remote/f.bin")}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "f.bin"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("hydrated content differs (got %d bytes, want %d)", len(got), size)
	}
	if opens.Load() == 0 {
		t.Error("the stream given to MountStream was never used")
	}
}
