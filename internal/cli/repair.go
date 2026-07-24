package cli

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/otherworld/nimbo/internal/transport"
)

// cmdRepair verifies (and optionally restores) a remote folder against an
// authoritative LOCAL copy. It is deliberately ADDITIVE and NON-DESTRUCTIVE: it
// can upload files that are missing on the server and create missing folders,
// but it NEVER deletes or modifies anything on the server unless explicitly told
// to overwrite size-mismatched files. Server-only files are reported and left
// untouched.
//
// Default is a read-only report (dry run). Use --apply to upload missing files,
// and --overwrite to also re-upload files whose size differs (local wins).
//
//	nimbo repair <local> [remote]              # verify only (no changes)
//	nimbo repair <local> [remote] --apply      # + upload missing files/dirs
//	nimbo repair <local> [remote] --apply --overwrite  # + fix size mismatches
func cmdRepair(ctx context.Context, args []string) error {
	var apply, overwrite bool
	var pos []string
	for _, a := range args {
		switch a {
		case "--apply":
			apply = true
		case "--overwrite":
			overwrite = true
		case "-h", "--help":
			fmt.Println("usage: nimbo repair <local> [remote] [--apply] [--overwrite]")
			return nil
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %q", a)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: nimbo repair <local> [remote] [--apply] [--overwrite]")
	}
	local := pos[0]
	remoteRoot := ""
	if len(pos) >= 2 {
		remoteRoot = strings.Trim(pos[1], "/")
	}
	li, err := os.Stat(local)
	if err != nil {
		return err
	}
	if !li.IsDir() {
		return fmt.Errorf("%s is not a directory", local)
	}

	client, acc, err := clientForDefault()
	if err != nil {
		return err
	}
	fmt.Printf("Verifying %s\n      vs  %s (%s)\n\n", local,
		"server:/"+remoteRoot, acc.ServerURL)

	// 1. Pull the server's entire subtree in one request, indexed by path
	// relative to remoteRoot.
	fmt.Println("Listing server… (one request, may take a moment for large trees)")
	entries, err := client.PropFindRecursive(ctx, remoteRoot)
	if err != nil {
		return fmt.Errorf("list server %q: %w", remoteRoot, err)
	}
	server := make(map[string]repairEntry, len(entries))
	for _, e := range entries {
		rel := relTo(e.Path, remoteRoot)
		if rel == "" {
			continue // the root itself
		}
		server[rel] = repairEntry{isDir: e.IsDir, size: e.Size}
	}

	// 2. Walk the authoritative local tree into the same shape.
	localTree := map[string]repairEntry{}
	werr := filepath.WalkDir(local, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if repairSkip(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(local, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			localTree[rel] = repairEntry{isDir: true}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		localTree[rel] = repairEntry{size: info.Size()}
		return nil
	})
	if werr != nil {
		return werr
	}

	// 3. Compare (pure; covered by repair_test.go).
	plan := classifyRepair(localTree, server)

	// 4. Report.
	fmt.Printf("\nScanned %d local files, %d folders. Server has %d entries.\n\n", plan.files, plan.dirs, len(server))
	matched := plan.files - len(plan.missingFiles) - len(plan.mismatched)
	fmt.Printf("  matched (present, same size):        %d\n", matched)
	fmt.Printf("  MISSING on server (would upload):    %d\n", len(plan.missingFiles))
	fmt.Printf("  missing folders (would create):      %d\n", len(plan.missingDirs))
	fmt.Printf("  size MISMATCH (local vs server):     %d\n", len(plan.mismatched))
	fmt.Printf("  extra on server (NOT touched):       %d\n", len(plan.extra))

	printList("MISSING on server", plan.missingFiles, func(rel string) string { return rel })
	printList("size MISMATCH", plan.mismatched, func(rel string) string {
		s := plan.mismSize[rel]
		return fmt.Sprintf("%s (local %s, server %s)", rel, humanSize(s[0]), humanSize(s[1]))
	})
	printList("extra on server (left untouched)", plan.extra, func(rel string) string { return rel })

	if !apply {
		fmt.Println()
		if len(plan.missingFiles)+len(plan.missingDirs)+len(plan.mismatched) == 0 {
			fmt.Println("✓ Server matches the local copy (by presence + size). Nothing to do.")
		} else {
			fmt.Println("Dry run — no changes made.")
			fmt.Println("Re-run with --apply to upload the missing files/folders" +
				ternary(len(plan.mismatched) > 0, " (and --overwrite to also fix the size mismatches).", "."))
		}
		return nil
	}

	// 5. Apply: create missing dirs (shallow first), then upload missing files,
	// then (if --overwrite) re-upload size-mismatched files. NEVER deletes.
	fmt.Println("\nApplying (additive only; nothing is deleted)…")
	for _, rel := range plan.missingDirs {
		full := joinRemote(remoteRoot, rel)
		if err := client.EnsureCollection(ctx, full); err != nil {
			fmt.Printf("  mkdir FAILED %s: %v\n", full, err)
		} else {
			fmt.Printf("  created folder %s\n", full)
		}
	}
	uploads := plan.missingFiles
	if overwrite {
		uploads = append(append([]string{}, plan.missingFiles...), plan.mismatched...)
	}
	var done, failed int
	for _, rel := range uploads {
		if err := uploadFile(ctx, client, filepath.Join(local, filepath.FromSlash(rel)), joinRemote(remoteRoot, rel)); err != nil {
			failed++
			fmt.Printf("  upload FAILED %s: %v\n", rel, err)
			continue
		}
		done++
		fmt.Printf("  uploaded %s\n", rel)
	}
	fmt.Printf("\nDone: %d uploaded, %d failed.", done, failed)
	if len(plan.mismatched) > 0 && !overwrite {
		fmt.Printf(" (%d size mismatches left as-is; pass --overwrite to fix them.)", len(plan.mismatched))
	}
	fmt.Println()
	return nil
}

// repairEntry is one node (file or dir) in a tree being compared.
type repairEntry struct {
	isDir bool
	size  int64
}

// repairPlan is the result of comparing the authoritative local tree against the
// server: what to add, what differs, and what the server has that local doesn't.
type repairPlan struct {
	missingFiles []string            // file local, absent (or a dir) on server -> upload
	missingDirs  []string            // dir local, absent (or a file) on server -> mkdir
	mismatched   []string            // file both sides, different size
	extra        []string            // file on server, absent locally (never touched)
	mismSize     map[string][2]int64 // rel -> [localSize, serverSize]
	files, dirs  int                 // local files/dirs scanned
}

// classifyRepair compares an authoritative local tree against the server (both
// keyed by remote-root-relative path). It is pure (no I/O) and additive: it only
// ever reports what to upload/create and what differs — it never plans a delete,
// so server-only entries are surfaced as "extra" and otherwise left alone.
func classifyRepair(local, server map[string]repairEntry) repairPlan {
	p := repairPlan{mismSize: map[string][2]int64{}}
	for rel, le := range local {
		if le.isDir {
			p.dirs++
			if se, ok := server[rel]; !ok || !se.isDir {
				p.missingDirs = append(p.missingDirs, rel)
			}
			continue
		}
		p.files++
		se, ok := server[rel]
		switch {
		case !ok || se.isDir:
			p.missingFiles = append(p.missingFiles, rel)
		case se.size != le.size:
			p.mismatched = append(p.mismatched, rel)
			p.mismSize[rel] = [2]int64{le.size, se.size}
		}
	}
	for rel, se := range server {
		if se.isDir {
			continue // a dir is "extra" only if truly empty; skip dir noise
		}
		if le, ok := local[rel]; !ok || le.isDir {
			p.extra = append(p.extra, rel)
		}
	}
	sort.Strings(p.missingFiles)
	sort.Strings(p.missingDirs)
	sort.Strings(p.mismatched)
	sort.Strings(p.extra)
	return p
}

// uploadFile ensures the remote parent folder exists, then PUTs the local file.
func uploadFile(ctx context.Context, client *transport.Client, localPath, remotePath string) error {
	if i := strings.LastIndex(remotePath, "/"); i > 0 {
		if err := client.EnsureCollection(ctx, remotePath[:i]); err != nil {
			return err
		}
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = client.Put(ctx, remotePath, f, info.Size())
	return err
}

// relTo returns full's path relative to root (both files-root-relative).
func relTo(full, root string) string {
	full = strings.Trim(full, "/")
	root = strings.Trim(root, "/")
	if root == "" {
		return full
	}
	if full == root {
		return ""
	}
	return strings.TrimPrefix(full, root+"/")
}

func joinRemote(root, rel string) string {
	return strings.Trim(strings.Trim(root, "/")+"/"+rel, "/")
}

// repairSkip filters OS/editor junk and Nimbo temp files from the comparison.
func repairSkip(name string) bool {
	switch {
	case name == ".nimbo" || name == ".sync" || name == "$RECYCLE.BIN":
		return true
	case name == "Thumbs.db" || name == "desktop.ini" || name == ".DS_Store":
		return true
	case strings.HasSuffix(name, ".nimbo-part") || strings.HasPrefix(name, "~$") || strings.HasPrefix(name, ".~lock."):
		return true
	}
	return false
}

// printList shows up to a cap of items so a huge diff doesn't flood the terminal.
func printList(title string, items []string, fmtItem func(string) string) {
	if len(items) == 0 {
		return
	}
	const maxShow = 40
	fmt.Printf("\n%s (%d):\n", title, len(items))
	for i, it := range items {
		if i == maxShow {
			fmt.Printf("  … and %d more\n", len(items)-maxShow)
			break
		}
		fmt.Printf("  %s\n", fmtItem(it))
	}
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
