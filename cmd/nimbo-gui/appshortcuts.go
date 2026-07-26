package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Keeping app-shortcut icons alive across a change of app identity.
//
// A Start-menu or taskbar .lnk stores its icon as an absolute path. Ours used
// to resolve inside the MSIX package data root
// (…\Packages\<PackageFamilyName>\LocalCache\Roaming\…), which is only stable
// while the package IDENTITY is: the signer's subject becomes the Publisher,
// the Publisher becomes part of the PackageFamilyName, and the
// PackageFamilyName names the data root. Moving from the self-signed dev cert
// to Azure Trusted Signing therefore handed the app a brand-new, empty data
// root and left every icon path already written into a shortcut pointing at a
// folder that no longer exists. (The same event emptied the WebView2 profile,
// which is why it shows up as "I had to sign in to the web apps again" — same
// cause, different symptom. A move between the direct-download and Microsoft
// Store builds, or into a white-label build, does exactly the same thing.)
//
// Start-menu entries recovered on their own, because autoCreateAppShortcut
// rewrites them whenever an app window opens. A taskbar PIN does not: it is
// Windows' own private copy of the .lnk, never rewritten, so its icon stayed
// dead — and Explorer's fallback, the target's icon, is blank because the
// target is an AppExecutionAlias shim with no icon resources.
//
// So: icons now live beside the shortcuts that reference them, in a folder
// whose path carries no package identity (appIconsDirFor), and
// repairAppShortcutIcons re-points any shortcut of ours that still names a
// path which no longer resolves.

// appIconsFolder is the subfolder of the Start-menu shortcut folder holding the
// per-app icons. Hidden on Windows (see hidePath) so it stays out of the Start
// menu; dot-prefixed so it still reads as private if that attribute is lost.
const appIconsFolder = ".icons"

// appIconsDirFor picks where per-app .ico files live: beside the Start-menu
// shortcuts that reference them. That folder is physically real even inside the
// package — the shell folders under %APPDATA%\Microsoft are exempt from MSIX
// AppData virtualization, unlike %APPDATA%\<brand>, which is redirected into
// the package data root — and it names no package identity, so it survives a
// re-signing, a Store build and a white-label rebrand alike. Without a Start
// menu (non-Windows, or no APPDATA) it falls back to the old config location.
func appIconsDirFor(startMenuDir, configDir string) string {
	if startMenuDir != "" {
		return filepath.Join(startMenuDir, appIconsFolder)
	}
	if configDir == "" {
		return ""
	}
	return filepath.Join(configDir, "appicons")
}

// ensureAppIconsDir creates the icon folder, hidden where the platform has such
// a concept: it sits inside the Start-menu shortcut folder, and an unhidden
// ".icons" would show up as a stray entry in the user's Start menu.
func ensureAppIconsDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	hidePath(dir)
	return nil
}

// appIDFromShortcutArgs extracts the Nextcloud app id a shortcut launches.
// Deliberately strict — the arguments must be exactly the "--app <id>" we
// write, with an id that passes appIDRe — because a match is what licenses the
// repair sweep to rewrite the file. Anything else is somebody else's shortcut.
func appIDFromShortcutArgs(args string) (string, bool) {
	id, ok := strings.CutPrefix(strings.TrimSpace(args), "--app ")
	if !ok {
		return "", false
	}
	id = strings.TrimSpace(id)
	if !appIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// isOurShortcutTarget reports whether a .lnk launches this app. Matched on the
// target's file NAME, not its full path: the packaged target is the
// AppExecutionAlias shim and a dev target the loose exe, and a stale pin may
// still name a location this install no longer uses — which is precisely the
// kind of shortcut the sweep exists to repair.
func isOurShortcutTarget(target, alias, selfExe string) bool {
	if target == "" {
		return false
	}
	base := filepath.Base(target)
	if alias != "" && strings.EqualFold(base, alias) {
		return true
	}
	return selfExe != "" && strings.EqualFold(base, filepath.Base(selfExe))
}

// migrateLegacyAppIcons copies already-generated icons from the old,
// package-bound cache into the new identity-free folder. It matters because it
// lets an existing install's shortcuts be repaired offline, in one pass at
// startup, instead of waiting to re-fetch every icon from the server (which
// needs a signed-in engine and would leave pins blank until then). Only the
// current icon style is worth carrying; older revs are dead weight. Copies
// never overwrite and never delete — the old folder is left as it was.
func migrateLegacyAppIcons(oldDir, newDir string) int {
	if oldDir == "" || newDir == "" || oldDir == newDir {
		return 0
	}
	entries, err := os.ReadDir(oldDir)
	if err != nil {
		return 0
	}
	cur := "." + appIconsRev + ".ico"
	n := 0
	for _, e := range entries {
		name := e.Name()
		// The icon, plus its ".fallback" marker if it has one — leaving that
		// behind would strand a brand-icon fallback as if it were the real thing.
		if e.IsDir() || !(strings.HasSuffix(name, cur) || strings.HasSuffix(name, cur+".fallback")) {
			continue
		}
		dst := filepath.Join(newDir, name)
		if _, err := os.Stat(dst); err == nil {
			continue // the new folder wins
		}
		b, err := os.ReadFile(filepath.Join(oldDir, name))
		if err != nil {
			continue
		}
		if err := ensureAppIconsDir(newDir); err != nil {
			return n
		}
		if err := writeFileAtomic(dst, b); err == nil {
			n++
		}
	}
	return n
}

// writeFileAtomic writes via a unique temp file in the same directory, so a
// reader (Explorer, LoadImageW) can never see a half-written icon.
func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ico-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
