package officelock

import "testing"

// The forward rule, measured on Microsoft 365 Apps 16.0.20228.20124 / Windows 11
// by opening documents with stem lengths 1..12 and 16 and observing the files
// Office actually created. It matches LibreOffice's GenerateMSOLockFileURL()
// byte for byte, which is the corroborating source.
func TestOwnerFile(t *testing.T) {
	cases := []struct{ doc, want string }{
		// Excel and PowerPoint: "~$" + the whole leaf name, never truncated.
		{"Budget Report Q3.xlsx", "~$Budget Report Q3.xlsx"},
		{"a.xlsx", "~$a.xlsx"},
		{"quarterly numbers.xlsm", "~$quarterly numbers.xlsm"},
		{"Deck.pptx", "~$Deck.pptx"},

		// Word drops leading characters once the STEM passes a threshold: 0 if the
		// stem is 6 or shorter, 1 at exactly 7, 2 from 8 up. A legacy 8.3 artefact.
		{"abcdef.docx", "~$abcdef.docx"},
		{"abcdefg.docx", "~$bcdefg.docx"},
		{"abcdefgh.docx", "~$cdefgh.docx"},
		{"abcdefghi.docx", "~$cdefghi.docx"},
		{"abcdefghijklmnop.docx", "~$cdefghijklmnop.docx"},
		{"abcdefghij.rtf", "~$cdefghij.rtf"},
		{"Report.doc", "~$Report.doc"},
		{"Annual Report.docx", "~$nual Report.docx"},

		// LibreOffice applies the Word rule to ODT as well.
		{"abcdefgh.odt", "~$cdefgh.odt"},

		// Formats that produce no owner file at all.
		{"legacy.xls", ""},
		{"data.csv", ""},
		{"notes.txt", ""},
		{"archive.zip", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := OwnerFile(c.doc); got != c.want {
			t.Errorf("OwnerFile(%q) = %q, want %q", c.doc, got, c.want)
		}
	}
}

func TestIsOwnerFile(t *testing.T) {
	yes := []string{"~$Budget.xlsx", "~$cdefgh.docx", "~$a.pptx"}
	no := []string{"Budget.xlsx", "~notarealone.docx", ".~lock.a.odt#", "~$", "a~$b.docx"}
	for _, n := range yes {
		if !IsOwnerFile(n) {
			t.Errorf("IsOwnerFile(%q) = false, want true", n)
		}
	}
	for _, n := range no {
		if IsOwnerFile(n) {
			t.Errorf("IsOwnerFile(%q) = true, want false", n)
		}
	}
}

// The inverse is NOT computable by string surgery for Word: the dropped
// characters are gone. Resolving one means applying the forward rule to every
// candidate in the directory — and refusing when more than one matches, because
// distinct documents genuinely collide onto a single owner name.
func TestDocumentResolvesByEnumeration(t *testing.T) {
	siblings := []string{
		"Annual Report.docx", "Budget Report Q3.xlsx", "abcdefg.docx",
		"notes.txt", "~$nual Report.docx",
	}

	if got, ok := Document("~$Budget Report Q3.xlsx", siblings); !ok || got != "Budget Report Q3.xlsx" {
		t.Errorf("Excel: got %q, %v", got, ok)
	}
	if got, ok := Document("~$nual Report.docx", siblings); !ok || got != "Annual Report.docx" {
		t.Errorf("Word (2 dropped): got %q, %v", got, ok)
	}
	if got, ok := Document("~$bcdefg.docx", siblings); !ok || got != "abcdefg.docx" {
		t.Errorf("Word (1 dropped): got %q, %v", got, ok)
	}
	if _, ok := Document("~$nothing here.docx", siblings); ok {
		t.Error("an owner file with no matching document should not resolve")
	}
	if _, ok := Document("Budget Report Q3.xlsx", siblings); ok {
		t.Error("a non-owner name should not resolve")
	}
}

// Two different documents can produce the SAME owner name. Guessing between them
// would mean locking a file the user never opened, so refuse.
func TestDocumentRefusesAmbiguity(t *testing.T) {
	// stem 7 drops 1 -> "cdefgh.docx"; stem 8 drops 2 -> "cdefgh.docx".
	siblings := []string{"0cdefgh.docx", "12cdefgh.docx"}
	if got, ok := Document("~$cdefgh.docx", siblings); ok {
		t.Errorf("ambiguous owner file resolved to %q; want a refusal", got)
	}

	// With only one of them present it is unambiguous again.
	if got, ok := Document("~$cdefgh.docx", []string{"0cdefgh.docx"}); !ok || got != "0cdefgh.docx" {
		t.Errorf("got %q, %v; want 0cdefgh.docx", got, ok)
	}
}

// LibreOffice's own lock file is exact in both directions — no truncation.
func TestLibreOfficeLockNames(t *testing.T) {
	if got := LibreLockFile("Budget Report Q3.xlsx"); got != ".~lock.Budget Report Q3.xlsx#" {
		t.Errorf("LibreLockFile = %q", got)
	}
	if got, ok := DocumentFromLibreLock(".~lock.Budget Report Q3.xlsx#"); !ok || got != "Budget Report Q3.xlsx" {
		t.Errorf("DocumentFromLibreLock = %q, %v", got, ok)
	}
	for _, bad := range []string{"Budget.xlsx", ".~lock.missing-hash", "~$Budget.xlsx", ".~lock.#"} {
		if _, ok := DocumentFromLibreLock(bad); ok {
			t.Errorf("DocumentFromLibreLock(%q) resolved; want refusal", bad)
		}
	}
}

// Case matters on the extension check: Windows filenames are case-insensitive
// and a real "Desktop.ini" once slipped past a case-sensitive filter here.
func TestOwnerFileIgnoresExtensionCase(t *testing.T) {
	// A 9-character stem, so the Word rule must fire (drop 2) despite the
	// upper-case extension — proving the format lookup is case-insensitive
	// rather than accidentally falling through to "no owner file".
	if got := OwnerFile("QUARTERLY.DOCX"); got != "~$ARTERLY.DOCX" {
		t.Errorf("OwnerFile(QUARTERLY.DOCX) = %q, want ~$ARTERLY.DOCX", got)
	}
	if got := OwnerFile("REPORT.DOCX"); got != "~$REPORT.DOCX" {
		t.Errorf("OwnerFile(REPORT.DOCX) = %q, want ~$REPORT.DOCX (6-char stem drops nothing)", got)
	}
	if got := OwnerFile("Sheet.XLSX"); got != "~$Sheet.XLSX" {
		t.Errorf("OwnerFile(Sheet.XLSX) = %q", got)
	}
}
