//go:build windows

package agent

import (
	"golang.org/x/sys/windows"
)

// denyWriteHandle is an open handle that refuses other writers.
type denyWriteHandle struct{ h windows.Handle }

func (d *denyWriteHandle) Close() error {
	if d == nil || d.h == windows.InvalidHandle {
		return nil
	}
	err := windows.CloseHandle(d.h)
	d.h = windows.InvalidHandle
	return err
}

// holdDenyWrite opens path so that anything else trying to open it for WRITING
// is refused, while readers still succeed.
//
// This — not the `~$` file — is what produces Office's native "locked for
// editing by …" dialog. Measured on Microsoft 365: a valid owner file beside a
// freely writable document produces no dialog at all, whereas a write-share
// denial produces one, and the owner file merely supplies the name. It is also
// app-agnostic: anything that opens the file for writing is refused, so it
// covers CAD, PSD and the rest, just without the pretty message.
//
// The share mode matches what Word and Excel themselves take on an open
// document — FILE_SHARE_READ, i.e. deny write AND deny delete.
//
// Two consequences the caller must handle:
//
//   - It fails when somebody already holds the file with a conflicting mode
//     (the local user got there first). That is not an error worth surfacing;
//     the file is already protected by whoever holds it.
//   - While WE hold it, OUR OWN downloader cannot replace the file either.
//     Anything that writes to a held path must release the handle first.
func holdDenyWrite(path string) (*denyWriteHandle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ, // readers welcome; writers and deleters refused
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return &denyWriteHandle{h: h}, nil
}

// fileOnlineOnly reports whether path is an on-demand placeholder whose data
// is not on this PC (FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS). An attributes-only
// query: it cannot hydrate the file, which holdDenyWrite's open would. A var so
// tests can stand in for the cloud filter.
var fileOnlineOnly = func(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return attrs&0x400000 != 0 // FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS
}
