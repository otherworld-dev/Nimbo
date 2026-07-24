//go:build windows

// Command odprobe is a headless harness for the on-demand (Cloud Files API)
// provider. It mounts a temp folder with a synthetic remote tree and exercises
// lazy directory population (FETCH_PLACEHOLDERS) + hydration (FETCH_DATA) by
// listing/reading the placeholders — the same operations Explorer performs —
// so the provider can be validated without the shell. Not part of the app.
package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/vfs"
)

// synthetic remote tree: path -> children; files carry content.
var tree = map[string][]child{
	"":    {{name: "hello.txt", file: true}, {name: "sub", dir: true}},
	"sub": {{name: "world.txt", file: true}},
}
var content = map[string]string{
	"hello.txt":     "hello from cloud\n",
	"sub/world.txt": "nested world\n",
}

// fileIDs maps an entry's identity (path) to a stable server oc:fileid. A rename
// keeps the same fileid under a new name — that's what down-sync matches on.
var fileIDs = map[string]string{
	"hello.txt":     "f-hello",
	"sub/world.txt": "f-world",
}

// fixedMTime keeps placeholder mtimes stable so the refresh test is driven by a
// size change, not an ever-advancing clock.
var fixedMTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

type child struct {
	name string
	dir  bool
	file bool
}

func main() {
	cfapi.Debug = func(format string, args ...any) {
		fmt.Printf("  [cfapi] "+format+"\n", args...)
	}
	if !cfapi.Supported() {
		fmt.Println("cfapi not supported on this system")
		os.Exit(1)
	}

	dir, err := os.MkdirTemp("", "odprobe-")
	if err != nil {
		panic(err)
	}
	fmt.Println("mount dir:", dir)

	list := func(rel string) []cfapi.PlaceholderInfo {
		var items []cfapi.PlaceholderInfo
		for _, c := range tree[rel] {
			id := c.name
			if rel != "" {
				id = rel + "/" + c.name
			}
			items = append(items, cfapi.PlaceholderInfo{
				Name:     c.name,
				IsDir:    c.dir,
				Size:     int64(len(content[id])),
				ModTime:  fixedMTime, // stable, so ETag (not mtime) must drive change detection
				Identity: []byte(id),
				FileID:   fileIDs[id],
				ETag:     etagOf(content[id]), // changes whenever content changes, even at same size
			})
		}
		return items
	}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		data := content[string(identity)]
		if offset >= int64(len(data)) {
			return nil, nil
		}
		end := offset + length
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		return []byte(data[offset:end]), nil
	}

	exe, _ := os.Executable()
	key, err := cfapi.Mount(dir, "NimboProbe", exe, hydrate, list)
	if err != nil {
		fmt.Println("Mount error:", err)
		os.Exit(1)
	}
	fmt.Println("mounted, connKey:", key)
	defer func() {
		cfapi.Unmount(dir, key)
		_ = os.RemoveAll(dir)
		fmt.Println("unmounted + cleaned")
	}()

	// 1) enumerate the ROOT -> should trigger FETCH_PLACEHOLDERS and reveal entries
	fmt.Println("\n== ReadDir(root) ==")
	if !waitDir(dir, 2) {
		fmt.Println("FAIL: root never populated")
		return
	}

	// 2) enumerate the SUBDIR. NOTE: lazy FETCH_PLACEHOLDERS only fires under the
	// real Explorer shell, not Go's os.ReadDir, so this is expected to stay empty
	// here — it's verified manually in Explorer.
	fmt.Println("\n== ReadDir(sub) (lazy; Explorer-only, expect empty headless) ==")
	waitDir(filepath.Join(dir, "sub"), 1)

	// 3) read a placeholder -> should trigger FETCH_DATA and hydrate
	fmt.Println("\n== ReadFile(hello.txt) ==")
	want := content["hello.txt"]
	if ok := waitFile(filepath.Join(dir, "hello.txt"), want); ok {
		fmt.Println("VERIFY=true (hydrated bytes match)")
	} else {
		fmt.Println("FAIL: hydration mismatch")
	}

	// 4) write-back: create a new file -> watcher should upload + mark in-sync
	fmt.Println("\n== write-back (create newnote.txt) ==")
	uploaded := make(chan string, 4)
	moved := make(chan string, 4)
	deleted := make(chan string, 4)
	var fidMu sync.Mutex
	fidStore := map[string]string{} // remote path -> fileid (down-sync rename detection)
	etStore := map[string]string{}  // remote path -> last-synced ETag (change detection)
	w, werr := vfs.New(context.Background(), dir, "", time.Minute, vfs.Ops{
		Upload: func(_ context.Context, local, remote string) error { uploaded <- remote; return nil },
		Mkdir:  func(_ context.Context, remote string) error { uploaded <- "DIR:" + remote; return nil },
		Delete: func(_ context.Context, remote string) error { deleted <- remote; return nil },
		Move:   func(_ context.Context, src, dst string) error { moved <- src + " -> " + dst; return nil },
		List:   func(rel string) ([]cfapi.PlaceholderInfo, error) { return list(rel), nil },
		RecordBaseline: func(remote, etag string) { fidMu.Lock(); etStore[remote] = etag; fidMu.Unlock() },
		Baseline: func(remote string) (string, bool) {
			fidMu.Lock()
			defer fidMu.Unlock()
			e, ok := etStore[remote]
			return e, ok
		},
		RecordFileID: func(remote, fid string) { fidMu.Lock(); fidStore[remote] = fid; fidMu.Unlock() },
		FileID: func(remote string) (string, bool) {
			fidMu.Lock()
			defer fidMu.Unlock()
			f, ok := fidStore[remote]
			return f, ok
		},
		DropFileID: func(remote string) { fidMu.Lock(); delete(fidStore, remote); fidMu.Unlock() },
		Log:        func(f string, a ...any) { fmt.Printf("  [vfs] "+f+"\n", a...) },
	})
	if werr != nil {
		fmt.Println("FAIL: watcher:", werr)
		return
	}
	defer w.Close()

	newPath := filepath.Join(dir, "newnote.txt")
	if err := os.WriteFile(newPath, []byte("written by user\n"), 0o644); err != nil {
		fmt.Println("write err:", err)
	}
	select {
	case r := <-uploaded:
		fmt.Printf("uploaded remote=%q\n", r)
		// After upload the watcher calls MarkInSync (just after Upload returns),
		// so poll briefly for the in-sync state to settle.
		ok := false
		for i := 0; i < 30; i++ {
			if ch, e := cfapi.Inspect(newPath); e == nil && !ch.NeedsUpload {
				ok = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Printf("post-upload marked in-sync (no re-upload): %v\n", ok)
	case <-time.After(6 * time.Second):
		fmt.Println("FAIL: no upload fired")
	}

	// 5) rename: newnote.txt -> renamed.txt -> expect a server MOVE
	fmt.Println("\n== rename (newnote.txt -> renamed.txt) ==")
	renPath := filepath.Join(dir, "renamed.txt")
	_ = os.Rename(newPath, renPath)
	select {
	case m := <-moved:
		fmt.Printf("moved %s\n", m) // expect "newnote.txt -> renamed.txt"
	case <-time.After(6 * time.Second):
		fmt.Println("FAIL: no move fired")
	}

	// 6) delete: remove renamed.txt -> expect a server DELETE (after debounce)
	fmt.Println("\n== delete (renamed.txt) ==")
	_ = os.Remove(renPath)
	select {
	case d := <-deleted:
		fmt.Printf("deleted %s\n", d) // expect "renamed.txt"
	case <-time.After(6 * time.Second):
		fmt.Println("FAIL: no delete fired")
	}

	// 7) down-sync: simulate the server gaining a file and losing one, then
	// reconcile -> the local placeholder tree should follow.
	fmt.Println("\n== down-sync (reconcile) ==")
	tree[""] = []child{{name: "sub", dir: true}, {name: "fromserver.txt", file: true}} // hello.txt removed
	content["fromserver.txt"] = "added on another device\n"
	fileIDs["fromserver.txt"] = "f-server" // so a later server rename is detectable
	w.Reconcile()
	names := map[string]bool{}
	if es, e := os.ReadDir(dir); e == nil {
		for _, x := range es {
			names[x.Name()] = true
		}
	}
	fmt.Printf("after reconcile: fromserver.txt=%v (want true), hello.txt=%v (want false)\n",
		names["fromserver.txt"], names["hello.txt"])

	// 8) pin / free-up-space on the new online-only placeholder.
	fmt.Println("\n== pin / dehydrate (fromserver.txt) ==")
	fsPath := filepath.Join(dir, "fromserver.txt")
	fmt.Printf("SetPinState pinned err=%v\n", cfapi.SetPinState(fsPath, true, false))
	fmt.Printf("SetPinState unpinned err=%v\n", cfapi.SetPinState(fsPath, false, false))
	_, _ = os.ReadFile(fsPath) // hydrate so there's content to drop
	time.Sleep(300 * time.Millisecond)
	fmt.Printf("Dehydrate err=%v\n", cfapi.Dehydrate(fsPath))

	// 9) modified-on-server refresh: hydrate fromserver.txt, change its content on
	// the "server", reconcile -> the local copy should refresh to the new content.
	fmt.Println("\n== refresh (server changed fromserver.txt) ==")
	if b, _ := os.ReadFile(fsPath); len(b) > 0 {
		fmt.Printf("before: %q\n", string(b))
	}
	content["fromserver.txt"] = "EDITED on another device, now longer\n" // different size
	w.Reconcile()
	time.Sleep(300 * time.Millisecond)
	got, _ := os.ReadFile(fsPath) // re-open -> should re-fetch the new content
	fmt.Printf("after:  %q (want the EDITED text)\n", string(got))

	// 10) down-sync RENAME: the server renames fromserver.txt -> moved.txt keeping
	// the same fileid. Reconcile should MOVE the local placeholder (preserving its
	// hydrated content), not delete+recreate it.
	fmt.Println("\n== down-sync rename (fromserver.txt -> moved.txt, same fileid) ==")
	want10 := content["fromserver.txt"]
	_, _ = os.ReadFile(fsPath) // ensure it's hydrated before the rename
	time.Sleep(200 * time.Millisecond)
	tree[""] = []child{{name: "sub", dir: true}, {name: "moved.txt", file: true}}
	content["moved.txt"] = content["fromserver.txt"]
	fileIDs["moved.txt"] = fileIDs["fromserver.txt"] // SAME fileid => it's a rename, not delete+add
	w.Reconcile()
	time.Sleep(400 * time.Millisecond)
	movedExists, oldGone, noServerOp := false, true, true
	if es, e := os.ReadDir(dir); e == nil {
		for _, x := range es {
			if x.Name() == "moved.txt" {
				movedExists = true
			}
			if x.Name() == "fromserver.txt" {
				oldGone = false
			}
		}
	}
	// A correct pull-rename must NOT generate a server delete or move.
	select {
	case d := <-deleted:
		noServerOp = false
		fmt.Printf("  UNEXPECTED server delete: %s\n", d)
	case m := <-moved:
		noServerOp = false
		fmt.Printf("  UNEXPECTED server move: %s\n", m)
	default:
	}
	mb, _ := os.ReadFile(filepath.Join(dir, "moved.txt"))
	fmt.Printf("moved.txt=%v (want true), fromserver.txt gone=%v (want true), no bounce-back=%v (want true)\n",
		movedExists, oldGone, noServerOp)
	fmt.Printf("content preserved: %v (%q)\n", string(mb) == want10, string(mb))

	// 11) ETag change detection: edit moved.txt to a DIFFERENT content of the SAME
	// byte length. The size/mtime heuristic can't see it (size unchanged, mtime
	// fixed) — only the ETag baseline catches it. Reconcile should still refresh.
	fmt.Println("\n== ETag refresh (same-size content change) ==")
	mvPath := filepath.Join(dir, "moved.txt")
	_, _ = os.ReadFile(mvPath) // hydrate first
	time.Sleep(200 * time.Millisecond)
	sameSize := strings.Repeat("Z", len(content["moved.txt"])-1) + "\n" // identical length, new content
	content["moved.txt"] = sameSize
	w.Reconcile()
	time.Sleep(300 * time.Millisecond)
	got11, _ := os.ReadFile(mvPath) // re-open -> re-fetch
	fmt.Printf("same-size edit caught by ETag: %v (%q)\n", string(got11) == sameSize, string(got11))
}

// waitDir lists path until it has at least want entries (population is async).
func waitDir(path string, want int) bool {
	for i := 0; i < 50; i++ {
		entries, err := os.ReadDir(path)
		if err == nil && len(entries) >= want {
			for _, e := range entries {
				fmt.Printf("  - %s (dir=%v)\n", e.Name(), e.IsDir())
			}
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// etagOf is a synthetic server ETag: it changes whenever content changes, even
// at the same byte length — so the probe can verify ETag-based change detection
// catches same-size edits that the size/mtime heuristic would miss.
func etagOf(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum64())
}

func waitFile(path, want string) bool {
	for i := 0; i < 50; i++ {
		b, err := os.ReadFile(path)
		if err == nil && strings.TrimRight(string(b), "\x00") != "" {
			fmt.Printf("  read %q\n", string(b))
			return string(b) == want
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
