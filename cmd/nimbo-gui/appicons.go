package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nfnt/resize"

	"github.com/otherworld/nimbo/internal/config"
)

// Per-app icons for app windows and Start-menu shortcuts. Nextcloud's theming
// app (bundled + enabled by default) composes a 512×512 PNG per app at
// /index.php/apps/theming/icon/<id> — ideal raster source, since the raw app
// icons are SVGs Go can't rasterize without heavy deps. The PNG is downscaled
// to the standard icon sizes and cached as <config>/appicons/<id>.ico; any
// failure (theming disabled, no Imagick) falls back to writing the embedded
// brand icon so the path always exists and everything stays functional.

var icoSizes = []int{16, 24, 32, 48, 64, 256}

// appIconsRev versions the generated icon style. Bumping it wipes the cache on
// next use and regenerates (background) every icon a shortcut references, so
// existing Start-menu pins pick up the new look at their unchanged paths.
// rev 2: Nimbo badge composited into the corner (PWA-style "via Nimbo" mark).
// rev 3: small frames encoded as classic DIBs — the shell's icon extractor
//        (taskbar pins, .lnk icons) renders PNG-compressed frames below 256px
//        as a blank page, even though LoadImageW copes.
const appIconsRev = "3"

// appIconPath is where an app's cached .ico lives. The rev is part of the
// FILENAME: a style bump then regenerates at a fresh path with no directory
// wipe (a wipe can silently fail while Explorer holds an icon for a pinned
// shortcut — observed in the field), and Windows' icon cache, which happily
// serves stale content for an unchanged path, is defeated outright.
func (a *App) appIconPath(id string) string {
	d, err := config.Resolve()
	if err != nil {
		return ""
	}
	return filepath.Join(d.Config, "appicons", sanitizeFileName(id)+"."+appIconsRev+".ico")
}

// brandBadge returns the Nimbo tile as an image for corner-badging, decoded
// once from the largest PNG frame of the embedded brand .ico (our generator
// emits PNG-payload ICOs, so frames are plain PNG).
var brandBadgeOnce sync.Once
var brandBadgeImg image.Image

func brandBadge() image.Image {
	brandBadgeOnce.Do(func() {
		b := navIconICO
		if len(b) < 6 {
			return
		}
		n := int(binary.LittleEndian.Uint16(b[4:6]))
		bestW, off, ln := -1, 0, 0
		for i := 0; i < n; i++ {
			e := 6 + 16*i
			if e+16 > len(b) {
				return
			}
			w := int(b[e])
			if w == 0 {
				w = 256
			}
			if w > bestW {
				bestW = w
				ln = int(binary.LittleEndian.Uint32(b[e+8 : e+12]))
				off = int(binary.LittleEndian.Uint32(b[e+12 : e+16]))
			}
		}
		if off <= 0 || off+ln > len(b) {
			return
		}
		img, err := png.Decode(bytes.NewReader(b[off : off+ln]))
		if err == nil {
			brandBadgeImg = img
		}
	})
	return brandBadgeImg
}

// badgeAppIcon composites the Nimbo tile into the bottom-right corner (~38%,
// like a browser's PWA badge) so windows, Start entries and taskbar pins all
// read as "this app, opened via Nimbo".
func badgeAppIcon(src image.Image) image.Image {
	badge := brandBadge()
	if badge == nil {
		return src
	}
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	bw := b.Dx() * 38 / 100
	if bw < 8 {
		return src // too small to badge legibly
	}
	scaled := resize.Resize(uint(bw), uint(bw), badge, resize.Lanczos3)
	m := b.Dx() / 64 // whisker of margin
	pos := image.Rect(b.Dx()-bw-m, b.Dy()-bw-m, b.Dx()-m, b.Dy()-m)
	draw.Draw(out, pos, scaled, image.Point{}, draw.Over)
	return out
}

// migrateAppIcons runs once per rev bump: best-effort removal of previous-rev
// files (current-rev names embed the rev, so a locked leftover costs disk, not
// correctness) and background regeneration of every icon a recorded Start-menu
// shortcut references.
func (a *App) migrateAppIcons(dir string) {
	revFile := filepath.Join(dir, ".rev")
	if b, err := os.ReadFile(revFile); err == nil && strings.TrimSpace(string(b)) == appIconsRev {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	cur := "." + appIconsRev + ".ico"
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			n := e.Name()
			if strings.HasSuffix(n, ".ico") && !strings.HasSuffix(n, cur) {
				_ = os.Remove(filepath.Join(dir, n)) // stale rev — best-effort tidy
			}
		}
	}
	_ = os.WriteFile(revFile, []byte(appIconsRev), 0o600)
	if d, err := config.Resolve(); err == nil {
		if s, err := d.LoadSettings(); err == nil {
			for id := range s.AppShortcuts {
				go a.ensureAppIcon(id, false)
			}
		}
	}
}

// migrateOnce gates the icon-cache rev migration to once per process (rev only
// changes across releases; concurrent first calls must not race the wipe).
var migrateOnce sync.Once

// ensureAppIcon makes sure the app's .ico exists, fetching + converting the
// server's theming PNG. wait=false runs best-effort for the NEXT open (the
// current window falls back to the brand icon); wait=true blocks (shortcut
// creation points a .lnk at the path, so it must exist now).
func (a *App) ensureAppIcon(id string, wait bool) {
	path := a.appIconPath(id)
	if path == "" || a.eng == nil {
		return
	}
	migrateOnce.Do(func() { a.migrateAppIcons(filepath.Dir(path)) })
	marker := path + ".fallback"
	if _, err := os.Stat(path); err == nil {
		if _, ferr := os.Stat(marker); ferr != nil {
			return // cached, and it's the real app icon
		}
		// Cached icon is the brand fallback (a past fetch failed) — retry so a
		// transient server error can't brand-icon the app forever.
	}
	fetch := func() {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return
		}
		if err := a.fetchAppIcon(id, path); err != nil {
			slog.Debug("app icon fetch failed; using brand icon", "app", id, "err", err)
			// Fallback re-encodes the brand image (shell-safe DIB frames) rather
			// than copying the raw embedded ico, whose all-PNG frames the shell
			// icon extractor can't render for .lnk/pin icons.
			if badge := brandBadge(); badge != nil {
				if err := writeIcoFromImage(path, badge); err == nil {
					_ = os.WriteFile(marker, nil, 0o600)
					return
				}
			}
			_ = os.WriteFile(path, navIconICO, 0o600) // last resort
			_ = os.WriteFile(marker, nil, 0o600)
			return
		}
		_ = os.Remove(marker) // real icon landed — clear any fallback flag
	}
	if wait {
		fetch()
	} else {
		go fetch()
	}
}

// fetchAppIcon downloads the theming PNG for an app and writes a multi-size ICO.
func (a *App) fetchAppIcon(id, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := a.absURL("/index.php/apps/theming/icon/" + id)
	req, err := a.eng.Client().NewRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := a.eng.Client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errStatus(resp.StatusCode)
	}
	src, err := png.Decode(resp.Body)
	if err != nil {
		return err
	}
	return writeIcoFromImage(path, badgeAppIcon(src)) // + "via Nimbo" corner badge
}

// writeIcoFromImage renders an image into a multi-size .ico at path
// (atomically, via a unique temp file — concurrent fetches must not share one).
func writeIcoFromImage(path string, src image.Image) error {
	var frames [][]byte
	for _, s := range icoSizes {
		img := resize.Resize(uint(s), uint(s), src, resize.Lanczos3)
		var frame []byte
		if s >= 256 {
			var b bytes.Buffer
			if err := png.Encode(&b, img); err != nil {
				return err
			}
			frame = b.Bytes()
		} else {
			frame = dibFrame(img, s)
		}
		frames = append(frames, frame)
	}
	ico := encodeICO(icoSizes, frames)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ico-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(ico); err != nil {
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

// dibFrame encodes one icon frame as a classic 32bpp DIB (BITMAPINFOHEADER +
// bottom-up straight-alpha BGRA + an all-clear AND mask). The shell's icon
// extractor requires this for sizes below 256 — it renders PNG-compressed
// small frames as a blank page (while LoadImageW copes), which broke taskbar
// pin icons.
func dibFrame(img image.Image, s int) []byte {
	var buf bytes.Buffer
	maskStride := ((s + 31) / 32) * 4
	binary.Write(&buf, binary.LittleEndian, uint32(40))   // biSize
	binary.Write(&buf, binary.LittleEndian, int32(s))     // biWidth
	binary.Write(&buf, binary.LittleEndian, int32(2*s))   // biHeight (XOR + AND)
	binary.Write(&buf, binary.LittleEndian, uint16(1))    // biPlanes
	binary.Write(&buf, binary.LittleEndian, uint16(32))   // biBitCount
	binary.Write(&buf, binary.LittleEndian, uint32(0))    // biCompression = BI_RGB
	binary.Write(&buf, binary.LittleEndian, uint32(s*s*4+maskStride*s)) // biSizeImage
	buf.Write(make([]byte, 16)) // XPels/YPels/ClrUsed/ClrImportant
	b := img.Bounds()
	for y := s - 1; y >= 0; y-- { // bottom-up
		for x := 0; x < s; x++ {
			// NRGBA = straight (non-premultiplied) alpha, which icon DIBs expect.
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	buf.Write(make([]byte, maskStride*s)) // AND mask: all visible; alpha governs
	return buf.Bytes()
}

type errStatus int

func (e errStatus) Error() string { return "server returned status " + http.StatusText(int(e)) }

// encodeICO packs PNG-encoded frames into a Vista+ PNG-payload .ico
// (same format the brand icon generator emits — shell/windows/appicon).
func encodeICO(sizes []int, pngs [][]byte) []byte {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&buf, binary.LittleEndian, uint16(len(sizes)))
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		w := s
		if w >= 256 {
			w = 0 // 0 encodes 256
		}
		buf.WriteByte(byte(w))
		buf.WriteByte(byte(w))
		buf.WriteByte(0) // palette
		buf.WriteByte(0) // reserved
		binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
		binary.Write(&buf, binary.LittleEndian, uint16(32)) // bpp
		binary.Write(&buf, binary.LittleEndian, uint32(len(pngs[i])))
		binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return buf.Bytes()
}

// sanitizeFileName strips characters Windows forbids in file names.
func sanitizeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '-'
		}
		if r < 0x20 {
			return '-'
		}
		return r
	}, s)
}
