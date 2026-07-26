package main

import (
	"os"
	"path/filepath"
	"testing"
)

// appIDFromShortcutArgs decides whether the icon-repair sweep is allowed to
// rewrite a .lnk, so the cases that matter are the ones it must refuse — a
// shortcut we don't own must never be touched.
func TestAppIDFromShortcutArgs(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"what we write", "--app deck", "deck"},
		{"leading/trailing space", "  --app deck  ", "deck"},
		{"dotted id", "--app external_index2", "external_index2"},

		{"empty", "", ""},
		{"flag only", "--app", ""},
		{"flag, no id", "--app ", ""},
		{"another flag", "--share C:\\x\\y.txt", ""},
		{"extra argument", "--app deck --profile evil", ""},
		{"quoted", `--app "deck"`, ""},
		{"path traversal", `--app ..\..\windows`, ""},
		{"space in id", "--app my app", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := appIDFromShortcutArgs(c.args)
			if c.want == "" {
				if ok {
					t.Fatalf("appIDFromShortcutArgs(%q) = %q, true; want refused", c.args, got)
				}
				return
			}
			if !ok {
				t.Fatalf("appIDFromShortcutArgs(%q) refused; want %q", c.args, c.want)
			}
			if got != c.want {
				t.Fatalf("appIDFromShortcutArgs(%q) = %q; want %q", c.args, got, c.want)
			}
		})
	}
}

// The sweep matches on the target's file NAME: the packaged target is the
// AppExecutionAlias shim and the dev target the loose exe, and a stale pin may
// still name a location this install no longer uses.
func TestIsOurShortcutTarget(t *testing.T) {
	const alias = "nimbo-app.exe"
	const self = `C:\Git\Nimbo\bin\nimbo-gui.exe`

	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"alias", `C:\Users\a\AppData\Local\Microsoft\WindowsApps\nimbo-app.exe`, true},
		{"alias, different case", `C:\Users\a\AppData\Local\Microsoft\WindowsApps\NIMBO-APP.EXE`, true},
		{"alias at a stale location", `D:\old\nimbo-app.exe`, true},
		{"the dev exe", self, true},

		{"empty", "", false},
		{"someone else", `C:\Program Files\Mozilla Firefox\firefox.exe`, false},
		{"the CLI, not the launcher", `C:\Users\a\AppData\Local\Microsoft\WindowsApps\nimbo.exe`, false},
		{"name is a prefix only", `C:\x\nimbo-app.exe.bak`, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isOurShortcutTarget(c.target, alias, self); got != c.want {
				t.Fatalf("isOurShortcutTarget(%q) = %v; want %v", c.target, got, c.want)
			}
		})
	}
}

// The whole point of the new location is that it carries no package identity —
// a path under …\Packages\<PFN>\… is what stranded every pinned icon when the
// signing identity changed.
func TestAppIconsDirForPrefersShortcutFolder(t *testing.T) {
	const startMenu = `C:\Users\a\AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Nimbo Apps`
	const cfg = `C:\Users\a\AppData\Roaming\nimbo`

	got := appIconsDirFor(startMenu, cfg)
	if want := filepath.Join(startMenu, appIconsFolder); got != want {
		t.Fatalf("appIconsDirFor = %q; want %q", got, want)
	}

	// No Start menu (non-Windows, or no APPDATA) → the old config location.
	got = appIconsDirFor("", cfg)
	if want := filepath.Join(cfg, "appicons"); got != want {
		t.Fatalf("appIconsDirFor(no start menu) = %q; want %q", got, want)
	}
	if got := appIconsDirFor("", ""); got != "" {
		t.Fatalf("appIconsDirFor(nothing) = %q; want empty", got)
	}
}

// Existing installs must be repairable offline: the icons they already have
// are copied into the new folder rather than re-fetched from the server.
func TestMigrateLegacyAppIcons(t *testing.T) {
	oldDir := t.TempDir()
	newDir := filepath.Join(t.TempDir(), "icons")

	cur := "." + appIconsRev + ".ico"
	write := func(dir, name, body string) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(oldDir, "deck"+cur, "deck-icon")
	write(oldDir, "files"+cur, "files-icon")
	write(oldDir, "mail"+cur+".fallback", "") // retry marker travels with its icon
	write(oldDir, "mail"+cur, "mail-icon")
	write(oldDir, "notes.1.ico", "stale-rev") // a dead style — leave it behind
	write(oldDir, ".rev", appIconsRev)
	write(newDir, "files"+cur, "already-here") // never clobber the new folder

	// deck, mail and mail's fallback marker — files is already in the new folder.
	if n := migrateLegacyAppIcons(oldDir, newDir); n != 3 {
		t.Fatalf("migrateLegacyAppIcons copied %d; want 3", n)
	}

	body := func(name string) string {
		b, err := os.ReadFile(filepath.Join(newDir, name))
		if err != nil {
			t.Fatalf("missing %s in new dir: %v", name, err)
		}
		return string(b)
	}
	if got := body("deck" + cur); got != "deck-icon" {
		t.Fatalf("deck icon = %q", got)
	}
	if got := body("files" + cur); got != "already-here" {
		t.Fatalf("existing icon was clobbered: %q", got)
	}
	body("mail" + cur + ".fallback")
	if _, err := os.Stat(filepath.Join(newDir, "notes.1.ico")); err == nil {
		t.Fatal("stale-rev icon was copied; want it left behind")
	}

	// Idempotent: a second sweep has nothing left to do.
	if n := migrateLegacyAppIcons(oldDir, newDir); n != 0 {
		t.Fatalf("second migrateLegacyAppIcons copied %d; want 0", n)
	}
	// Same folder (non-Windows, where there is no separate icon location).
	if n := migrateLegacyAppIcons(oldDir, oldDir); n != 0 {
		t.Fatalf("same-dir migrateLegacyAppIcons copied %d; want 0", n)
	}
}
