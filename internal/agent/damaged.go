package agent

import (
	"log/slog"
	"path"

	"github.com/otherworld/nimbo/internal/engine"
)

// A server copy whose bytes do not match the checksum stored with it is
// damaged, typically by an upload made while another program was writing the
// file (Deck #691: Outlook with an attached .pst). Downloading it again only
// brings back the same bytes, and on a 24 GB file each try costs minutes, so
// that exact copy is skipped until the server's copy changes. The record is
// kept in memory: a restart tries once more, which is also how a copy the
// server served badly once, rather than holds badly, gets another go.

func damagedKey(pk, rel string) string { return pk + "\x00" + rel }

// noteDamaged records that the server's copy of rel at etag failed its
// checksum, and says so once per copy.
func (e *Engine) noteDamaged(pk, rel, etag string) {
	e.damagedMu.Lock()
	if e.damaged == nil {
		e.damaged = make(map[string]string)
	}
	prev, seen := e.damaged[damagedKey(pk, rel)]
	e.damaged[damagedKey(pk, rel)] = etag
	e.damagedMu.Unlock()
	if !seen || prev != etag {
		e.toast("Damaged on the server: "+path.Base(rel), damagedCopyMsg, "")
	}
}

// skipDamaged drops downloads of copies known to be damaged: the same path at
// the same server etag. A copy that has changed since is downloaded again,
// and its record goes. Skipped paths are returned so the pass treats them as
// unfinished: their folders keep being re-listed rather than stamped clean.
func (e *Engine) skipDamaged(pk string, actions []engine.Action, remote map[string]engine.RemoteState) (kept []engine.Action, skipped []string) {
	e.damagedMu.Lock()
	defer e.damagedMu.Unlock()
	if len(e.damaged) == 0 {
		return actions, nil
	}
	kept = actions[:0:0]
	for _, a := range actions {
		if a.Kind == engine.ActDownload {
			key := damagedKey(pk, a.Path)
			if etag, ok := e.damaged[key]; ok {
				if r, listed := remote[a.Path]; listed && r.ETag == etag {
					skipped = append(skipped, a.Path)
					slog.Debug("skipping a download whose server copy is damaged", "path", a.Path, "etag", etag)
					continue
				}
				delete(e.damaged, key) // the server's copy changed: try it
			}
		}
		kept = append(kept, a)
	}
	return kept, skipped
}

const damagedCopyMsg = "the copy on the server is damaged (its content doesn't match the checksum stored with it), " +
	"so it wasn't downloaded. It will be tried again when it changes on the server. " +
	"Restore an earlier version from the Nextcloud web page, or upload it again from the computer that has a good copy."
