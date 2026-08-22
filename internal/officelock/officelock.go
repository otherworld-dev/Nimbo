// Package officelock knows the lock-file conventions of Microsoft Office and
// LibreOffice: what each app names the sidecar file it creates while a document
// is open, and how to get back from that sidecar to the document.
//
// It exists so the live-sync watcher and the on-demand watcher — two separate
// code paths — agree on the rules, and so those rules can be tested without a
// filesystem or a copy of Office.
//
// The forward rule below was measured on Microsoft 365 Apps 16.0.20228.20124 /
// Windows 11 (documents with stem lengths 1..12 and 16, observing what Office
// actually created) and matches LibreOffice's GenerateMSOLockFileURL() exactly.
package officelock

import (
	"path"
	"strings"
)

const ownerPrefix = "~$"

const (
	librePrefix = ".~lock."
	libreSuffix = "#"
)

// wordLike are the formats whose owner file has leading characters DROPPED.
// LibreOffice extends Microsoft's rule to ODT when it writes MSO lock files.
var wordLike = map[string]bool{".doc": true, ".docx": true, ".rtf": true, ".odt": true}

// plainOwner are the formats whose owner file is simply "~$" + the leaf name.
// Note .xls is absent on purpose: Office creates no owner file for it, which
// LibreOffice's source calls out explicitly.
var plainOwner = map[string]bool{
	".xlsx": true, ".xlsm": true, ".ods": true,
	".pptx": true, ".ppt": true, ".odp": true,
}

// OwnerFile returns the Office owner-file name that opening doc would create,
// or "" for a document type that produces none.
//
// For Word-family formats the name is "~$" plus the leaf with its first d
// characters removed, where d is 0 for a stem of 6 or fewer, 1 at exactly 7, and
// 2 from 8 upward — a legacy 8.3 artefact that keeps the owner file's stem from
// growing past 8. Note the threshold is measured on the STEM but the characters
// come off the FULL leaf name.
func OwnerFile(doc string) string {
	doc = path.Base(strings.ReplaceAll(doc, `\`, "/"))
	if doc == "" || doc == "." || doc == "/" {
		return ""
	}
	ext := strings.ToLower(path.Ext(doc))
	stem := len(doc) - len(ext) // path.Ext includes the dot, so this IS the stem
	switch {
	case plainOwner[ext]:
		return ownerPrefix + doc
	case wordLike[ext]:
		d := 0
		switch {
		case stem >= 8:
			d = 2
		case stem == 7:
			d = 1
		}
		if d > len(doc) {
			return ownerPrefix + doc
		}
		return ownerPrefix + doc[d:]
	}
	return ""
}

// IsOwnerFile reports whether name looks like an Office owner file.
func IsOwnerFile(name string) bool {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	return strings.HasPrefix(name, ownerPrefix) && len(name) > len(ownerPrefix)
}

// Document resolves an owner-file name back to the document that produced it,
// given the names present in the same directory.
//
// This has to enumerate: for Word-family formats the dropped characters are gone
// and the mapping is genuinely many-to-one, so there is nothing to invert. When
// two candidates map to the same owner file the answer is a refusal, not a
// guess — guessing would lock a document the user never opened.
func Document(owner string, siblings []string) (string, bool) {
	owner = path.Base(strings.ReplaceAll(owner, `\`, "/"))
	if !IsOwnerFile(owner) {
		return "", false
	}
	var found string
	n := 0
	for _, s := range siblings {
		s = path.Base(strings.ReplaceAll(s, `\`, "/"))
		if IsOwnerFile(s) {
			continue // an owner file is never itself a document
		}
		if o := OwnerFile(s); o != "" && strings.EqualFold(o, owner) {
			found = s
			n++
		}
	}
	if n != 1 {
		return "", false // none, or ambiguous
	}
	return found, true
}

// LibreLockFile returns LibreOffice's lock-file name for doc: the full leaf name
// wrapped in ".~lock." and "#", never truncated.
func LibreLockFile(doc string) string {
	doc = path.Base(strings.ReplaceAll(doc, `\`, "/"))
	if doc == "" {
		return ""
	}
	return librePrefix + doc + libreSuffix
}

// DocumentFromLibreLock inverts LibreLockFile. Unlike the Office owner file this
// one is exact, so no directory listing is needed.
func DocumentFromLibreLock(name string) (string, bool) {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	if !strings.HasPrefix(name, librePrefix) || !strings.HasSuffix(name, libreSuffix) {
		return "", false
	}
	doc := name[len(librePrefix) : len(name)-len(libreSuffix)]
	if doc == "" {
		return "", false
	}
	return doc, true
}
