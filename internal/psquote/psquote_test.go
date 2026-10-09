package psquote

import "testing"

func TestQuote(t *testing.T) {
	cases := map[string]string{
		`C:\Users\Adam`:         `'C:\Users\Adam'`,
		`C:\Users\O'Brien`:      `'C:\Users\O''Brien'`,
		`C:\a$b` + "`c":         "'C:\\a$b`c'", // literal, no PowerShell expansion
		`'; Remove-Item C:\`:    `'''; Remove-Item C:\'`,
		"a\u2019; calc; \u2018": "'a\u2019\u2019; calc; \u2018\u2018'", // PowerShell reads these as ' too
		"\u201A\u201B":          "'\u201A\u201A\u201B\u201B'",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}
