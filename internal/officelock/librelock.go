package officelock

import (
	"strings"
	"time"
)

// LibreOffice's lock file is plain UTF-8 text: five comma-separated fields
// terminated by a semicolon, no trailing newline.
//
//	OOOUSERNAME,SYSUSERNAME,LOCALHOST,EDITTIME,USERURL;
//
// From LockFileCommon::GenerateOwnEntry(). A real-world sample:
//
//	Dedoimedo,HOST/roger,HOST,03.04.2016 17:10,file:///C:/Users/roger/AppData/Roaming/LibreOffice/4;
//
// Unlike Microsoft's owner file this one is human-readable and exact in both
// directions, and LibreOffice treats its mere PRESENCE as the lock — no OS
// write-share test needed. That makes it the one editor we can warn without
// holding a handle.

// libreEscape escapes the characters LibreOffice's own parser treats as
// structural. Its reader rejects a backslash followed by anything else, so only
// these three may be escaped.
func libreEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`)
	return r.Replace(s)
}

// LibreLockContent builds the body of a `.~lock.<name>#` file.
//
// user is the display name LibreOffice shows in its "Document in Use" dialog;
// sysUser and host identify the machine. editTime is formatted exactly as
// LibreOffice writes it — "DD.MM.YYYY HH:MM", zero-padded, in LOCAL time — so a
// real LibreOffice reading our file sees what it expects.
func LibreLockContent(user, sysUser, host string, editTime time.Time, userURL string) string {
	return strings.Join([]string{
		libreEscape(user),
		libreEscape(sysUser),
		libreEscape(host),
		editTime.Format("02.01.2006 15:04"),
		libreEscape(userURL),
	}, ",") + ";"
}

// ParseLibreLockUser pulls the display name out of a `.~lock.` file, falling
// back to the system user when the first field is empty — LibreOffice leaves it
// blank when the user has set no name in Tools > Options, which shows up as a
// leading comma.
func ParseLibreLockUser(content string) (string, bool) {
	s := strings.TrimSuffix(strings.TrimSpace(content), ";")
	if s == "" {
		return "", false
	}
	fields := splitLibre(s)
	if len(fields) == 0 {
		return "", false
	}
	if fields[0] != "" {
		return fields[0], true
	}
	if len(fields) > 1 && fields[1] != "" {
		return fields[1], true
	}
	return "", false
}

// splitLibre splits on unescaped commas, honouring the backslash escaping.
func splitLibre(s string) []string {
	var out []string
	var cur strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
		case r == ',':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	return out
}
