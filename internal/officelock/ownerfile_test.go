package officelock

import (
	"bytes"
	"strings"
	"testing"
)

// The layouts below are transcribed from real hexdumps of files Word, Excel and
// PowerPoint wrote on Microsoft 365 Apps 16.0.20228.20124 for the user "Adam".
func TestOwnerFileContentWord(t *testing.T) {
	b := OwnerFileContent("Report.docx", "Adam")
	if len(b) != 162 {
		t.Fatalf("len = %d, want 162 (Word layout)", len(b))
	}
	if b[0] != 4 {
		t.Errorf("b[0] = %d, want the character count 4", b[0])
	}
	if got := string(b[1:5]); got != "Adam" {
		t.Errorf("ANSI name = %q, want Adam", got)
	}
	// Word ZERO-pads; a space here would be the Excel layout.
	if b[5] != 0x00 || b[53] != 0x00 {
		t.Errorf("padding = %#x/%#x, want zeroes", b[5], b[53])
	}
	if b[54] != 4 || b[55] != 0 {
		t.Errorf("u16 length at 54 = %#x %#x, want 04 00", b[54], b[55])
	}
	if want := []byte{'A', 0, 'd', 0, 'a', 0, 'm', 0}; !bytes.Equal(b[56:64], want) {
		t.Errorf("UTF-16 name at 56 = % x, want % x", b[56:64], want)
	}
}

func TestOwnerFileContentExcel(t *testing.T) {
	b := OwnerFileContent("Budget.xlsx", "Adam")
	if len(b) != 165 {
		t.Fatalf("len = %d, want 165 (Excel layout)", len(b))
	}
	if b[0] != 4 {
		t.Errorf("b[0] = %d, want 4", b[0])
	}
	if got := string(b[1:5]); got != "Adam" {
		t.Errorf("ANSI name = %q", got)
	}
	// Excel SPACE-pads, and its length field is one byte later than Word's.
	if b[5] != 0x20 || b[54] != 0x20 {
		t.Errorf("padding = %#x/%#x, want spaces", b[5], b[54])
	}
	if b[55] != 4 || b[56] != 0 {
		t.Errorf("u16 length at 55 = %#x %#x, want 04 00", b[55], b[56])
	}
	if want := []byte{'A', 0, 'd', 0, 'a', 0, 'm', 0}; !bytes.Equal(b[57:65], want) {
		t.Errorf("UTF-16 name at 57 = % x, want % x", b[57:65], want)
	}
}

// PowerPoint is the Excel layout with exactly one 0x00 after the ANSI name.
func TestOwnerFileContentPowerPoint(t *testing.T) {
	b := OwnerFileContent("Deck.pptx", "Adam")
	if len(b) != 165 {
		t.Fatalf("len = %d, want 165", len(b))
	}
	if b[5] != 0x00 {
		t.Errorf("b[5] = %#x, want 0x00 (PowerPoint's marker byte)", b[5])
	}
	if b[6] != 0x20 {
		t.Errorf("b[6] = %#x, want space padding to resume", b[6])
	}
}

func TestOwnerFileContentSkipsUnsupported(t *testing.T) {
	for _, doc := range []string{"legacy.xls", "data.csv", "notes.txt", "photo.png", ""} {
		if got := OwnerFileContent(doc, "Adam"); got != nil {
			t.Errorf("OwnerFileContent(%q) returned %d bytes, want nil", doc, len(got))
		}
	}
}

// Both length fields count characters and cap at 52; a longer name is truncated
// rather than overflowing the fixed-size record.
func TestOwnerFileContentCapsLongNames(t *testing.T) {
	long := strings.Repeat("x", 80)
	b := OwnerFileContent("Report.docx", long)
	if len(b) != 162 {
		t.Fatalf("len = %d, want 162", len(b))
	}
	if b[0] != 52 {
		t.Errorf("b[0] = %d, want the 52-character cap", b[0])
	}
	if got, ok := ParseOwnerFileUser(b); !ok || len(got) != 52 {
		t.Errorf("round-trip = %q (%d chars), %v; want 52 chars", got, len(got), ok)
	}
}

func TestOwnerFileRoundTrip(t *testing.T) {
	for _, doc := range []string{"Report.docx", "Budget.xlsx", "Deck.pptx", "notes.odt"} {
		b := OwnerFileContent(doc, "Bob Smith")
		got, ok := ParseOwnerFileUser(b)
		if !ok || got != "Bob Smith" {
			t.Errorf("%s: round-trip = %q, %v; want Bob Smith", doc, got, ok)
		}
	}
}

// A file we do not understand must read as unknown, never as garbage: the name
// ends up in a dialog a colleague reads.
func TestParseOwnerFileUserRejectsJunk(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"wrong size":       make([]byte, 100),
		"zero length byte": make([]byte, 162),
		"length too big":   append([]byte{99}, make([]byte, 161)...),
	}
	for name, b := range cases {
		if got, ok := ParseOwnerFileUser(b); ok {
			t.Errorf("%s: parsed as %q, want a refusal", name, got)
		}
	}

	// The two length fields disagreeing is the strongest signal of a layout we
	// have misidentified — Word's u16 sits where Excel's padding does.
	b := OwnerFileContent("Report.docx", "Adam")
	b[54] = 9
	if got, ok := ParseOwnerFileUser(b); ok {
		t.Errorf("mismatched lengths parsed as %q, want a refusal", got)
	}
}
