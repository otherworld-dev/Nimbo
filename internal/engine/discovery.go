package engine

import (
	"context"
	"strings"
	"sync"

	"github.com/otherworld/nimbo/internal/transport"
)

// scanWorkers bounds how many directory PROPFINDs run concurrently during a
// crawl. Discovery is network-latency-bound (one round-trip per directory), so
// fanning out turns an N×latency sequential walk into roughly N/scanWorkers.
// Deliberately modest: every PROPFIND costs the server filecache DB queries,
// and 16 workers cold-crawling a large tree drove a small Nextcloud host to
// exhaustion (MariaDB pinned, then "server has gone away" aborting the scan).
// 4 keeps a cold crawl parallel while leaving the server room to breathe.
const scanWorkers = 4

// parseChecksumSHA1 extracts the SHA1 hex digest from an oc:checksums value such
// as "SHA1:abc… MD5:def…", lowercased. Returns "" if no SHA1 token is present.
func parseChecksumSHA1(checksums string) string {
	for _, tok := range strings.Fields(checksums) {
		if strings.HasPrefix(strings.ToUpper(tok), "SHA1:") {
			return strings.ToLower(tok[len("SHA1:"):])
		}
	}
	return ""
}

// PropFinder is the one slice of transport.Client the remote scan needs — a
// seam so tests can drive crawls from an in-memory tree instead of a WebDAV
// server. *transport.Client satisfies it unchanged.
type PropFinder interface {
	PropFind(ctx context.Context, path string, depth int) ([]transport.Entry, error)
}

// ScanOpts bundles RemoteScan's optional inputs; the zero value is a plain raw
// crawl (no pruning, no filtering, no name decoding).
type ScanOpts struct {
	// Base enables the ETag prune: a directory whose baseline etag matches the
	// server's is reconstructed from the baseline instead of being fetched.
	Base map[string]BaselineState
	// Skip is consulted with each entry's root-relative (decoded) path; a match
	// is omitted AND not descended into. Must use the same predicate as the
	// post-scan ignore filter so it never prunes a path the diff would act on.
	Skip func(rel string) bool
	// OnEncrypted is invoked once per end-to-end encrypted folder encountered;
	// E2EE folders are skipped entirely (opaque ciphertext to us).
	OnEncrypted func(rel string)
	// Esc decodes escaped server names (X.nimboesc -> X) so the remote map is
	// keyed by LOCAL names. Nil when escaping is inactive.
	Esc *Escaper
	// Checkpoint persists each directory listing as it is fetched and serves it
	// back on a later attempt when the ETag still matches — so a failed crawl
	// resumes instead of restarting cold. Nil disables checkpointing.
	Checkpoint ScanCheckpoint
}

// ScanCheckpoint caches raw directory listings across scan attempts (Deck
// #231). Implementations are best-effort: they must swallow their own I/O
// errors (a broken cache only costs re-fetching, never a failed scan).
// Implementations must be safe for concurrent use — the scan calls Load and
// Save from multiple workers at once.
type ScanCheckpoint interface {
	// Load returns dir's cached children iff the stored etag equals
	// expectedETag. expectedETag == "" must never match.
	Load(dir, expectedETag string) ([]transport.Entry, bool)
	// Save stores dir's children under etag — the etag dir's PARENT listing
	// reported before dir was fetched. That observation strictly precedes the
	// fetch, so an interleaved server change leaves a stale-OLD stored etag:
	// the next attempt misses and refetches. Never a stale hit. etag == ""
	// must be a no-op.
	Save(dir, etag string, children []transport.Entry)
}

// scanItem is one queued directory: its raw files-root-relative path, the ETag
// its parent's listing reported ("" for the root — unknown, so the root is
// always fetched fresh and never saved), and whether checkpointing is disabled
// for this subtree. noCache is set at a mount point ('M' in oc:permissions)
// and inherited by every descendant: external-storage etags are mtime-derived
// — the one place "a change bumps every ancestor's etag" is unreliable — and a
// stale cached listing there could replay as false server-side deletions. The
// baseline prune fails SAFE on a stale etag (it replays the baseline); a
// checkpoint hit replays an old server listing, so it must not run where the
// invariant is weak.
type scanItem struct {
	dir     string
	etag    string
	noCache bool
}

// RemoteScan walks the remote tree under root (a files-root-relative path; ""
// means the whole files root) and returns the current remote state keyed by
// paths *relative to root*, matching the keying used by LocalScan and the
// baseline.
//
// It exploits a Nextcloud invariant for speed: a directory's ETag changes
// whenever anything in its subtree changes. So when a directory's ETag matches
// the baseline, the entire subtree is known-unchanged and is reconstructed from
// the baseline instead of being fetched. (With an empty baseline, e.g. first
// run, it naturally performs a full crawl.)
func RemoteScan(ctx context.Context, c PropFinder, root string, opts ScanOpts) (map[string]RemoteState, error) {
	root = strings.Trim(root, "/")
	base, skip, onEncrypted, esc := opts.Base, opts.Skip, opts.OnEncrypted, opts.Esc
	out := make(map[string]RemoteState)

	// Index baseline entries by their parent directory once, so an unchanged
	// subtree is reconstructed in O(subtree) instead of scanning the whole
	// baseline per pruned directory (which was O(dirs × entries) overall).
	childrenOf := make(map[string][]string, len(base))
	for k := range base {
		p := parentOf(k)
		childrenOf[p] = append(childrenOf[p], k)
	}

	var (
		mu      sync.Mutex
		cond    = sync.NewCond(&mu)
		queue   = []scanItem{{dir: root}} // directories still to list (LIFO)
		pending = 1                       // queued-or-in-flight directories; scan ends at 0
		failed  error
	)

	cp := opts.Checkpoint

	// process lists one directory (from cache when the checkpoint validates,
	// else the network), records its children, and queues changed
	// subdirectories. PROPFIND and checkpoint I/O run without the lock held;
	// only the in-memory bookkeeping is serialised.
	process := func(it scanItem) {
		var (
			entries []transport.Entry
			err     error
			cached  bool
		)
		if cp != nil && !it.noCache && it.etag != "" {
			entries, cached = cp.Load(it.dir, it.etag)
		}
		if !cached {
			entries, err = c.PropFind(ctx, it.dir, 1)
			if err == nil && cp != nil && !it.noCache && it.etag != "" {
				cp.Save(it.dir, it.etag, childrenOnly(entries, it.dir))
			}
		}

		mu.Lock()
		defer mu.Unlock()
		defer cond.Broadcast() // wake waiters: new work queued, or pending changed
		pending--
		if err != nil {
			if failed == nil {
				failed = err
			}
			return
		}
		for _, e := range entries {
			full := strings.Trim(e.Path, "/")
			if full == it.dir {
				continue // the directory itself, not a child
			}
			rel := relTo(full, root)
			// Decode an escaped server name (X.nimboesc -> X) so the remote map is
			// keyed by LOCAL names; the diff, baseline and ignore rules then all match
			// on X as normal. No-op when escaping is inactive.
			if esc != nil {
				rel, _ = esc.Decode(rel)
			}
			if skip != nil && skip(rel) {
				continue // ignored/excluded — don't record it and don't descend
			}
			if e.IsDir && e.IsEncrypted {
				if onEncrypted != nil {
					onEncrypted(rel)
				}
				continue // E2EE folder — leave untouched on both sides
			}
			out[rel] = RemoteState{
				Path:     rel,
				IsDir:    e.IsDir,
				ETag:     e.ETag,
				FileID:   e.FileID,
				Size:     e.Size,
				SHA1:     parseChecksumSHA1(e.Checksums),
				ReadOnly: e.ServerReadOnly(),
			}
			if !e.IsDir {
				continue
			}
			if b, ok := base[rel]; ok && b.IsDir && b.RemoteETag == e.ETag {
				addBaselineSubtree(out, base, childrenOf, rel) // unchanged subtree — reuse baseline
			} else {
				queue = append(queue, scanItem{
					dir:     full,
					etag:    e.ETag,
					noCache: it.noCache || strings.Contains(e.Permissions, "M"),
				})
				pending++
			}
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < scanWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				for len(queue) == 0 && pending > 0 && failed == nil {
					cond.Wait()
				}
				if failed != nil || (len(queue) == 0 && pending == 0) {
					mu.Unlock()
					return
				}
				it := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				mu.Unlock()
				process(it)
			}
		}()
	}
	wg.Wait()

	if failed != nil {
		return nil, failed
	}
	return out, nil
}

// childrenOnly strips the directory's own row from a depth-1 response (the
// server includes it "when present") so cached listings hold children only;
// replay tolerates either shape because process() drops an exact-match row.
func childrenOnly(entries []transport.Entry, dir string) []transport.Entry {
	out := make([]transport.Entry, 0, len(entries))
	for _, e := range entries {
		if strings.Trim(e.Path, "/") == dir {
			continue
		}
		out = append(out, e)
	}
	return out
}

// relTo converts a files-root-relative path to one relative to root.
func relTo(full, root string) string {
	if root == "" {
		return full
	}
	if full == root {
		return ""
	}
	return strings.TrimPrefix(full, root+"/")
}

// parentOf returns the parent directory of a "/"-separated relative path, or ""
// for a top-level entry.
func parentOf(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return ""
}

// addBaselineSubtree copies every baseline descendant of prefix into out,
// synthesising RemoteState from the stored baseline. It walks the pre-built
// parent→children index so each call costs O(subtree), not O(whole baseline).
// The prefix directory itself is already present (with its freshly fetched
// ETag), so only descendants are added.
func addBaselineSubtree(out map[string]RemoteState, base map[string]BaselineState, childrenOf map[string][]string, prefix string) {
	for _, child := range childrenOf[prefix] {
		b := base[child]
		out[child] = RemoteState{
			Path:   child,
			IsDir:  b.IsDir,
			ETag:   b.RemoteETag,
			FileID: b.RemoteFileID,
			Size:   b.LocalSize, // sizes match at sync time; good enough for planning
		}
		if b.IsDir {
			addBaselineSubtree(out, base, childrenOf, child)
		}
	}
}
