// Package psquote renders strings as PowerShell single-quoted literals, for
// the places that build a PowerShell command or script out of paths and names.
//
// A single-quoted literal does no expansion, so a $ or a backtick stays as it
// is, and the only escape is a doubled quote. PowerShell reads the typographic
// quotes ‘ ’ ‚ ‛ as single quotes as well as the apostrophe, so all five are
// doubled: a path or name holding one of them (a file name from the server, a
// login) must never be able to end the string and run the rest as code.
package psquote

import "strings"

var quotes = strings.NewReplacer(
	"'", "''",
	"‘", "‘‘",
	"’", "’’",
	"‚", "‚‚",
	"‛", "‛‛",
)

// Quote returns s as a single-quoted PowerShell literal.
func Quote(s string) string { return "'" + Escape(s) + "'" }

// Escape returns s ready to go between single quotes the caller writes itself.
func Escape(s string) string { return quotes.Replace(s) }
