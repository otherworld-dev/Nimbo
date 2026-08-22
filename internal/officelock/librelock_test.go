package officelock

import (
	"testing"
	"time"
)

func TestLibreLockContent(t *testing.T) {
	when := time.Date(2016, 4, 3, 17, 10, 0, 0, time.UTC)
	got := LibreLockContent("Dedoimedo", "HOST/roger", "HOST", when,
		"file:///C:/Users/roger/AppData/Roaming/LibreOffice/4")
	want := "Dedoimedo,HOST/roger,HOST,03.04.2016 17:10,file:///C:/Users/roger/AppData/Roaming/LibreOffice/4;"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The date format is LibreOffice's, not Go's default: zero-padded DD.MM.YYYY
// and a 24-hour clock. Getting it wrong would put a nonsense date in the dialog
// a colleague reads.
func TestLibreLockContentDateFormat(t *testing.T) {
	when := time.Date(2026, 1, 5, 9, 7, 0, 0, time.UTC)
	got := LibreLockContent("A", "b", "c", when, "d")
	if want := "A,b,c,05.01.2026 09:07,d;"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Structural characters must be escaped or the file parses as the wrong number
// of fields — and LibreOffice's reader rejects a backslash before anything else.
func TestLibreLockContentEscaping(t *testing.T) {
	when := time.Date(2026, 1, 5, 9, 7, 0, 0, time.UTC)
	got := LibreLockContent(`Smith, John`, `dom\user`, "host;1", when, "u")
	want := `Smith\, John,dom\\user,host\;1,05.01.2026 09:07,u;`
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestParseLibreLockUser(t *testing.T) {
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"normal", "Dedoimedo,HOST/roger,HOST,03.04.2016 17:10,file:///x;", "Dedoimedo", true},
		{"no display name falls back to the system user",
			",HOST/roger,HOST,03.04.2016 17:10,file:///x;", "HOST/roger", true},
		{"escaped comma in the name",
			`Smith\, John,b,c,05.01.2026 09:07,d;`, "Smith, John", true},
		{"empty", "", "", false},
		{"only a semicolon", ";", "", false},
		{"both name fields empty", ",,c,d,e;", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseLibreLockUser(c.in)
			if ok != c.ok || got != c.want {
				t.Errorf("= (%q, %v), want (%q, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

// What we write must be readable by what we read.
func TestLibreLockRoundTrip(t *testing.T) {
	when := time.Date(2026, 8, 15, 1, 30, 0, 0, time.UTC)
	for _, name := range []string{"Adam", "Smith, John", `back\slash`, "semi;colon"} {
		body := LibreLockContent(name, "sys", "host", when, "file:///x")
		got, ok := ParseLibreLockUser(body)
		if !ok || got != name {
			t.Errorf("%q: round-trip gave (%q, %v)", name, got, ok)
		}
	}
}
