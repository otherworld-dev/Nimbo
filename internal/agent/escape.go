package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/policy"
	"github.com/otherworld/nimbo/internal/transfer"
)

// EscapedExtensions returns the file extensions currently opted in to
// server-forbidden-name escaping (e.g. [".htaccess"]).
func (e *Engine) EscapedExtensions() []string {
	if s, err := e.dirs.LoadSettings(); err == nil {
		return append([]string(nil), s.EscapeExtensions...)
	}
	return nil
}

// CanEscape reports whether a blocked file could be synced by opting its extension
// into escaping (forbidden by name/extension, not by a character).
func (e *Engine) CanEscape(base string) bool { return e.escaper.Load().WouldEscape(base) }

// SetAllowedFilenames rebuilds the forbidden-name matcher (and the escaper that
// depends on it) with a new user allow-list, then re-syncs — so permitting or
// un-permitting a name takes effect immediately instead of on next launch.
func (e *Engine) SetAllowedFilenames(allowed []string) {
	names := append(append([]string{}, e.caps.Files.ForbiddenFilenames...), e.caps.Files.BlacklistedFiles...)
	f := engine.NewForbidden(names, e.caps.Files.ForbiddenBasenames, e.caps.Files.ForbiddenCharacters, e.caps.Files.ForbiddenExtensions, allowed)
	e.forbidden.Store(f)
	e.escaper.Store(engine.NewEscaper(f, e.EscapedExtensions(), ""))
	e.TriggerFullSync()
}

// normalizeEscapeExt lowercases and ensures a single leading dot.
func normalizeEscapeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

// EnableEscaping opts the given extension into escaping so forbidden files of that
// type sync (stored on the server under a marker name) instead of being blocked.
// Persists the choice, swaps in a fresh escaper, and kicks a sync so the now-
// syncable files upload. No-op if already enabled; refused if policy disables it.
func (e *Engine) EnableEscaping(ext string) error {
	ext = normalizeEscapeExt(ext)
	if ext == "" {
		return nil
	}
	if policy.Load().DisableNameEscaping {
		return fmt.Errorf("name escaping is disabled by administrator policy")
	}
	var already bool
	var exts []string
	if err := e.dirs.UpdateSettings(func(s *config.Settings) {
		for _, x := range s.EscapeExtensions {
			if strings.EqualFold(x, ext) {
				already = true
				return
			}
		}
		s.EscapeExtensions = append(s.EscapeExtensions, ext)
		exts = s.EscapeExtensions
	}); err != nil {
		return err
	}
	if already {
		return nil
	}
	e.escaper.Store(engine.NewEscaper(e.forbidden.Load(), exts, ""))
	e.TriggerFullSync()
	slog.Info("name escaping enabled", "ext", ext)
	return nil
}

// DisableEscaping stops escaping the given extension and cleans up: it deletes our
// marker copies (X<suffix>) from the server so nothing dangles, first re-downloading
// any whose local original went missing so it NEVER deletes the only copy. The files
// revert to device-only and re-surface as blocked. Returns the number of server
// copies removed.
func (e *Engine) DisableEscaping(ctx context.Context, ext string) (int, error) {
	ext = normalizeEscapeExt(ext)
	if ext == "" {
		return 0, nil
	}
	esc := e.escaper.Load()
	suffix := esc.Suffix()
	pairs, err := e.Pairs()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, p := range pairs {
		// Raw scan (no escaper) so we see the X<suffix> names exactly as stored.
		// Checkpoint-backed under the same key as the adopt scan (both are raw
		// whole-tree listings), so a sweep after a recent adopt/scan is warm —
		// cold, this crawl ran silently for over an hour on a real account.
		// NOTE: the pair BASELINE must NOT be used here: it is keyed by DECODED
		// names, and a baseline-pruned subtree would hide the escaped copies
		// this sweep exists to find.
		opts := engine.ScanOpts{}
		var cp *scanCheckpoint
		if st, serr := e.getStore(); serr == nil {
			cp = newScanCheckpoint(st, "vfs-adopt:"+strings.Trim(p.RemoteRoot, "/"))
			opts.Checkpoint = cp
		}
		remote, err := engine.RemoteScan(ctx, e.client, p.RemoteRoot, opts)
		if cp != nil {
			cp.logSummary()
		}
		if err != nil {
			return removed, fmt.Errorf("scan %s: %w", p.RemoteRoot, err)
		}
		for rel, st := range remote {
			if st.IsDir {
				continue
			}
			base := path.Base(rel)
			if !strings.HasSuffix(base, suffix) {
				continue
			}
			decoded := strings.TrimSuffix(base, suffix)
			// Only OUR escaped copies: the decoded name must be a forbidden name of
			// the extension being disabled. This leaves a genuine *.<suffix> file
			// (whose decoded name isn't forbidden) untouched.
			if !strings.EqualFold(path.Ext(decoded), ext) {
				continue
			}
			if _, bad := e.forbidden.Load().Check(decoded); !bad {
				continue
			}
			localRel := path.Join(path.Dir(rel), decoded)
			localPath := filepath.Join(p.LocalDir, filepath.FromSlash(localRel))
			serverPath := path.Join(p.RemoteRoot, rel)
			if _, serr := os.Stat(localPath); serr != nil {
				// Local original missing — recover it from the marker copy before
				// deleting, so the file is never lost.
				if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
					return removed, err
				}
				if _, derr := transfer.Download(ctx, e.client, serverPath, localPath); derr != nil {
					slog.Warn("disable-escaping: could not recover local copy; leaving server file", "path", localRel, "err", derr)
					continue // keep the only copy on the server
				}
			}
			if err := e.client.Delete(ctx, serverPath); err != nil {
				slog.Warn("disable-escaping: server delete failed", "path", rel, "err", err)
				continue
			}
			removed++
		}
	}
	// Persist the removal + swap in a fresh escaper.
	var kept []string
	if err := e.dirs.UpdateSettings(func(s *config.Settings) {
		kept = s.EscapeExtensions[:0]
		for _, x := range s.EscapeExtensions {
			if !strings.EqualFold(x, ext) {
				kept = append(kept, x)
			}
		}
		s.EscapeExtensions = kept
	}); err != nil {
		return removed, err
	}
	e.escaper.Store(engine.NewEscaper(e.forbidden.Load(), kept, ""))
	e.TriggerFullSync() // re-surface the reverted files as blocked
	slog.Info("name escaping disabled", "ext", ext, "serverCopiesRemoved", removed)
	return removed, nil
}
