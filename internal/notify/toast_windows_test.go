//go:build windows

package notify

import (
	"strings"
	"testing"
)

// TestSanitizeToastTextNeutralisesInjection asserts that the characters go-toast
// would let PowerShell act on are removed from untrusted toast text. See the
// comment on sanitizeToastText: the toast XML is built inside a double-quoted
// PowerShell here-string, so `$`, backtick, a line-leading `"@`, and control
// characters are the live injection surface.
func TestSanitizeToastTextNeutralisesInjection(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"subexpression", `$(Start-Process calc.exe)`},
		{"variable", `hello $env:USERNAME world`},
		{"backtick", "back`tick`escape"},
		{"heredoc-terminator", "line one\r\n\"@\r\nStart-Process calc"},
		{"cdata-break", "note ]]><script> end"},
		{"control-chars", "a\x00b\x07c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeToastText(c.in)
			if strings.ContainsAny(got, "$`") {
				t.Errorf("sanitizeToastText(%q) = %q still contains $ or backtick", c.in, got)
			}
			if strings.Contains(got, "]]>") {
				t.Errorf("sanitizeToastText(%q) = %q still contains a CDATA break", c.in, got)
			}
			for _, r := range got {
				if r < 0x20 {
					t.Errorf("sanitizeToastText(%q) = %q still contains control char %#x", c.in, got, r)
				}
			}
		})
	}
}

// TestSanitizeToastTextKeepsOrdinaryText makes sure normal notification text —
// including punctuation and non-ASCII — survives unharmed.
func TestSanitizeToastTextKeepsOrdinaryText(t *testing.T) {
	ok := []string{
		"Alice shared \"Q3 report.xlsx\" with you",
		"Réunion à 14h — salle café ☕",
		"file (1).txt renamed to file-2.txt",
		"100% complete: 3/3 synced",
	}
	for _, s := range ok {
		if got := sanitizeToastText(s); got != s {
			t.Errorf("sanitizeToastText(%q) = %q, want unchanged", s, got)
		}
	}
}

// A toast whose link is one of our own "action=…" strings must activate the
// app, which routes it through dispatchToastActivation. Raised as a protocol
// launch, Windows was asked to open "action=inuse" as a URL, so clicking the
// conflict and file-in-use toasts never reached the tab they were about
// (Deck #714). A real link still opens as a protocol launch.
func TestToastActionLinkActivatesTheApp(t *testing.T) {
	x := buildToastXML("Sync conflict", "a.pst needs your decision", "action=conflicts")
	if !strings.Contains(x, `activationType="foreground" launch="action=conflicts"`) {
		t.Errorf("action link not a foreground activation: %s", x)
	}
	x = buildToastXML("Talk", "hi", "https://cloud.example.com/call/abc")
	if !strings.Contains(x, `activationType="protocol" launch="https://cloud.example.com/call/abc"`) {
		t.Errorf("web link not a protocol launch: %s", x)
	}
}
