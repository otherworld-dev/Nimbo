package officelock

import (
	"encoding/binary"
	"path"
	"strings"
	"unicode/utf16"
)

// Office owner-file layouts, measured byte-for-byte on Microsoft 365 Apps
// 16.0.20228.20124 / Windows 11 by hexdumping the files Word, Excel and
// PowerPoint actually wrote.
//
// All three start with a one-byte character count, then the user name in the
// system ANSI codepage, then the SAME count again as a little-endian uint16,
// then the name a second time in UTF-16LE. What differs is the padding byte and
// the offsets — and the offsets differ by ONE between Word and Excel, which will
// silently corrupt a reader that assumes a single layout.
const (
	wordSize     = 162  // Word / RTF / ODT
	wordU16LenAt = 54   // uint16 length
	wordWideAt   = 56   // UTF-16LE name
	wordPad      = 0x00 // zero-padded

	excelSize     = 165 // Excel and PowerPoint
	excelU16LenAt = 55  // one byte later than Word
	excelWideAt   = 57
	excelPad      = 0x20 // SPACE-padded, not zero

	maxOwnerNameChars = 52 // both length fields cap here; Office refuses longer
)

// pptLike are the formats that use the Excel layout but write a single 0x00
// immediately after the ANSI name before the space padding begins.
var pptLike = map[string]bool{".pptx": true, ".ppt": true, ".odp": true}

// OwnerFileContent builds the bytes of the owner file that opening doc would
// produce, naming user as the person who has it open.
//
// This is what turns a generic "the file is in use" into "locked for editing by
// Bob Smith": the owner file is only the NAME carrier. On its own it produces no
// dialog at all — that comes from the document failing a write-share test — so
// this is necessary but not sufficient. Returns nil for a document type that has
// no owner file.
func OwnerFileContent(doc, user string) []byte {
	ext := strings.ToLower(path.Ext(path.Base(doc)))
	if !wordLike[ext] && !plainOwner[ext] {
		return nil
	}
	name := []rune(user)
	if len(name) > maxOwnerNameChars {
		name = name[:maxOwnerNameChars]
	}
	n := len(name)

	size, lenAt, wideAt, pad := wordSize, wordU16LenAt, wordWideAt, byte(wordPad)
	if !wordLike[ext] {
		size, lenAt, wideAt, pad = excelSize, excelU16LenAt, excelWideAt, byte(excelPad)
	}

	buf := make([]byte, size)
	if pad == excelPad {
		// Excel/PowerPoint pad the ANSI field with spaces and the tail with
		// UTF-16 spaces (the repeating 0x20 0x00 pair).
		for i := range buf {
			if i%2 == 0 || i < wideAt {
				buf[i] = excelPad
			}
		}
	}

	buf[0] = byte(n)
	// ANSI: the low byte of each UTF-16 unit, which is what LibreOffice writes and
	// what Office reads back for any Latin-1 name. A non-ASCII name may not
	// round-trip through a DBCS codepage — both length fields count CHARACTERS,
	// not bytes, so that case is knowingly approximate.
	for i, r := range name {
		buf[1+i] = byte(r)
	}
	if pptLike[ext] {
		buf[1+n] = 0x00 // PowerPoint's one distinguishing byte
	}
	binary.LittleEndian.PutUint16(buf[lenAt:], uint16(n))
	for i, u := range utf16.Encode(name) {
		off := wideAt + i*2
		if off+1 >= size {
			break
		}
		binary.LittleEndian.PutUint16(buf[off:], u)
	}
	return buf
}

// ParseOwnerFileUser reads the user name out of an owner file.
//
// Dispatch is on SIZE, because the length field sits at a different offset in
// each layout: 162 bytes is Word, 165 is Excel or PowerPoint. Both length fields
// are cross-checked, so a truncated or foreign file reads as unknown rather than
// as garbage.
func ParseOwnerFileUser(b []byte) (string, bool) {
	var lenAt, wideAt int
	switch len(b) {
	case wordSize:
		lenAt, wideAt = wordU16LenAt, wordWideAt
	case excelSize:
		lenAt, wideAt = excelU16LenAt, excelWideAt
	default:
		return "", false
	}
	n := int(b[0])
	if n < 1 || n > maxOwnerNameChars {
		return "", false
	}
	if int(binary.LittleEndian.Uint16(b[lenAt:])) != n {
		return "", false // the two counts disagree: not a layout we understand
	}
	if wideAt+n*2 > len(b) {
		return "", false
	}
	u := make([]uint16, n)
	for i := 0; i < n; i++ {
		u[i] = binary.LittleEndian.Uint16(b[wideAt+i*2:])
	}
	return string(utf16.Decode(u)), true
}
