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
