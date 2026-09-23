package main

import (
	"log/slog"
	"strings"

	"github.com/otherworld/nimbo/internal/transfer"
	"github.com/otherworld/nimbo/internal/transport"
)

// sameBytesAsServer reports whether the local file is byte-identical to the
// server's copy, despite a differing ETag.
//
// Nextcloud bumps a file's ETag for metadata-only changes, and taking or
// releasing a files_lock lock is one — so is a tag, a comment or a favourite.
// The on-demand upload path used to read any ETag difference as "the server
// changed" and park the server's copy as a conflicted copy. On 2026-08-16 that
// produced two files with an identical SHA1, from a document nobody had edited
// on either side.
//
// The live diff learned this lesson separately (engine.sameContentAsBaseline);
// this is the on-demand half of the same rule. Conservative: no server checksum,
// or an unreadable local file, means "assume changed" and take the safe path.
func sameBytesAsServer(localPath string, ent transport.Entry) bool {
	remote := ent.ContentSHA1()
	if remote == "" {
		return false
	}
	local, err := transfer.SHA1File(localPath)
	if err != nil || local == "" {
		return false
	}
	if strings.EqualFold(local, remote) {
		slog.Info("on-demand: ETag moved but the bytes match; not a conflict", "path", localPath)
		return true
	}
	return false
}

// serverEditedSince reports whether cur, the server's copy now, is a different
// version from the one a local edit started from: the baseline ETag base, and
// baseKey, the content key recorded with it (transport.ContentKey).
//
// A new ETag alone is not enough. files_lock moves the ETag when a colleague
// locks and again when they unlock, so an edit made while they had the file
// open used to park their untouched copy as a conflicted copy (GitHub #7,
// VM-reproduced 2026-09-23). A matching content key proves the server still
// holds the version we started from. Conservative: an unknown key on either
// side leaves the ETag to decide, as before.
func serverEditedSince(base, baseKey string, cur transport.Entry) bool {
	if cur.ETag == base {
		return false
	}
	k := cur.ContentKey()
	return k == "" || k != baseKey
}
