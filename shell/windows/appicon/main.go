// Command appicon generates the Nimbo application icon used for the
// Explorer navigation-pane entry (and available for packaging). Run:
//
//	go run .   # writes ../../../cmd/nimbo-gui/assets/nimbo.ico
//
// Committed; re-run only to change the artwork.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/otherworld/nimbo/internal/brand"
)

// Nimbo's own palette: a GOLDEN nimbus cloud (think Dragon Ball's Kinto'un)
// on an indigo night-sky tile — deliberately nothing like the white-on-blue
// clouds of Nextcloud (#0082C9), OneDrive and Dropbox.
//
// These four slots are filled in main() from the active brand's accent: the
// stock Nimbo accent reproduces the exact hand-tuned values; any other
// (white-label) accent derives a matching two-tone palette (see newPalette).
var (
	indigoTop color.RGBA // tile gradient top
	indigoBot color.RGBA // tile gradient bottom
	goldLight color.RGBA // nimbus body
	goldDeep  color.RGBA // nimbus underside (cel shadow)
)

func main() {
	// White-label: the icon's colours come from the active brand's accent. The
	// optional -accent flag overrides it to preview or generate a reseller's
	// icon without editing brand.json.
	accent := flag.String("accent", "", "brand accent hex (#RRGGBB) to use instead of brand.json")
	flag.Parse()
	hex := brand.Current.AccentHex
	if *accent != "" {
		hex = *accent
	}
	p := newPalette(hex)
	indigoTop, indigoBot, goldLight, goldDeep = p.tileTop, p.tileBot, p.cloudLight, p.cloudDeep

	// `go run . logos <dir>` writes the MSIX PNG logo set; default writes the ICO.
	args := flag.Args()
	if len(args) >= 2 && args[0] == "logos" {
		writeLogos(args[1])
		return
	}
	out := filepath.Join("..", "..", "..", "cmd", "nimbo-gui", "assets")
	if err := os.MkdirAll(out, 0o755); err != nil {
		panic(err)
	}
	writeICO(filepath.Join(out, "nimbo.ico"), []int{16, 24, 32, 48, 64, 256})
}

// stockAccent is Nimbo's own brand accent; a build using it keeps the exact
// hand-tuned icon, while any other accent derives its palette from that colour.
const stockAccent = "#5856E0"

// palette holds the icon's four working colours: the tile gradient (top/bottom)
// and the nimbus body/underside drawn over it.
type palette struct {
	tileTop, tileBot, cloudLight, cloudDeep color.RGBA
}

// newPalette returns the icon palette for a brand accent. The stock accent maps
// to the canonical gold-on-indigo. Any other accent becomes the tile (brightened
// at the top, deepened at the bottom) with a near-complementary, warm-biased
// cloud over it — the same light-body / dark-underside contrast that reads as
// the flying nimbus, in the reseller's colour.
func newPalette(accentHex string) palette {
	if normalizeHex(accentHex) == normalizeHex(stockAccent) {
		return palette{
			tileTop:    color.RGBA{0x6F, 0x6A, 0xF2, 0xff},
			tileBot:    color.RGBA{0x4A, 0x3E, 0xD2, 0xff},
			cloudLight: color.RGBA{0xFF, 0xD5, 0x5F, 0xff},
			cloudDeep:  color.RGBA{0xEF, 0xA5, 0x3A, 0xff},
		}
	}
	h, s, l := rgbToHSL(parseHex(accentHex))
	// Cloud hue: the opposite side of the wheel, nudged warm so an indigo-family
	// accent still yields gold rather than a cold lemon.
	ch := math.Mod(h+165, 360)
	return palette{
		tileTop:    hslToRGB(h, clamp01(s*0.97), clamp01(l+0.10)),
		tileBot:    hslToRGB(h, clamp01(s*1.02), clamp01(l-0.14)),
		cloudLight: hslToRGB(ch, 0.95, 0.69),
		cloudDeep:  hslToRGB(math.Mod(ch-12+360, 360), 0.84, 0.58),
	}
}

// normalizeHex lowercases a hex colour and strips a leading '#' for comparison.
func normalizeHex(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "#")
}

// parseHex parses "#RRGGBB" (or "RRGGBB"); anything malformed falls back to the
// stock indigo so the generator never panics on a bad brand.json.
func parseHex(s string) color.RGBA {
	s = normalizeHex(s)
	v, err := strconv.ParseUint(s, 16, 32)
	if len(s) != 6 || err != nil {
		return color.RGBA{0x58, 0x56, 0xE0, 0xff}
	}
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
}

// rgbToHSL converts an opaque colour to hue (0–360), saturation and lightness (0–1).
func rgbToHSL(c color.RGBA) (h, s, l float64) {
	r, g, b := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255
	max := math.Max(r, math.Max(g, b))
	min := math.Min(r, math.Min(g, b))
	l = (max + min) / 2
	d := max - min
	if d == 0 {
		return 0, 0, l
	}
	if l > 0.5 {
		s = d / (2 - max - min)
	} else {
		s = d / (max + min)
	}
	switch max {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return math.Mod(h*60+360, 360), s, l
}

// hslToRGB is the inverse of rgbToHSL.
func hslToRGB(h, s, l float64) color.RGBA {
	if s == 0 {
		v := clampByte(l)
		return color.RGBA{v, v, v, 0xff}
	}
	var q float64
	if l < 0.5 {
		q = l * (1 + s)
	} else {
		q = l + s - l*s
	}
	p := 2*l - q
	hk := h / 360
	comp := func(t float64) uint8 {
		t = math.Mod(t+1, 1)
		switch {
		case t < 1.0/6:
			return clampByte(p + (q-p)*6*t)
		case t < 1.0/2:
			return clampByte(q)
		case t < 2.0/3:
			return clampByte(p + (q-p)*(2.0/3-t)*6)
		default:
			return clampByte(p)
		}
	}
	return color.RGBA{comp(hk + 1.0/3), comp(hk), comp(hk - 1.0/3), 0xff}
}

func clampByte(f float64) uint8 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 255
	default:
		return uint8(math.Round(f * 255))
	}
}

func clamp01(f float64) float64 { return math.Max(0, math.Min(1, f)) }

// writeLogos emits the PNG logos an MSIX AppxManifest references.
func writeLogos(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	logos := map[string]int{
		"Square44x44Logo.png":   44,
		"Square150x150Logo.png": 150,
		"Square310x310Logo.png": 310,
		"StoreLogo.png":         50,
	}
	// Target-size + UNPLATED variants. Windows 11 draws a coloured plate behind
	// a packaged app's taskbar/title-bar icon UNLESS an "_altform-unplated"
	// asset exists (transparent BackgroundColor only suppresses the Start tile
	// plate). Our artwork already carries its own rounded tile, so the unplated
	// asset is just the icon as-is. Resolved at runtime via the package
	// resources.pri (makepri, in package.ps1).
	for _, s := range []int{16, 24, 32, 48, 256} {
		logos[fmt.Sprintf("Square44x44Logo.targetsize-%d.png", s)] = s
		logos[fmt.Sprintf("Square44x44Logo.targetsize-%d_altform-unplated.png", s)] = s
	}
	for name, size := range logos {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, draw(size)); err != nil {
			panic(err)
		}
		f.Close()
	}
}

func writeICO(path string, sizes []int) {
	var pngs [][]byte
	for _, s := range sizes {
		var b bytes.Buffer
		if err := png.Encode(&b, draw(s)); err != nil {
			panic(err)
		}
		pngs = append(pngs, b.Bytes())
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint16(0))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(len(sizes)))
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		w := s
		if w >= 256 {
			w = 0
		}
		buf.WriteByte(byte(w))
		buf.WriteByte(byte(w))
		buf.WriteByte(0)
		buf.WriteByte(0)
		binary.Write(&buf, binary.LittleEndian, uint16(1))
		binary.Write(&buf, binary.LittleEndian, uint16(32))
		binary.Write(&buf, binary.LittleEndian, uint32(len(pngs[i])))
		binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
}

// draw renders Nimbo's tile: an indigo-violet gradient rounded square carrying
// the golden NIMBUS — a billowy cumulus, lumpy top and bottom with a cel-shaded
// underside and a trailing puff, like the flying nimbus it's named for.
func draw(size int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	f := float64(size)
	m := f * 0.08   // margin
	rad := f * 0.22 // corner radius
	// Rounded-square tile with a vertical indigo->violet gradient.
	for y := 0; y < size; y++ {
		t := (float64(y) - m) / (f - 2*m)
		if t < 0 {
			t = 0
		}
		if t > 1 {
			t = 1
		}
		row := lerpColor(indigoTop, indigoBot, t)
		for x := 0; x < size; x++ {
			if inRoundRect(float64(x)+0.5, float64(y)+0.5, m, m, f-m, f-m, rad) {
				img.Set(x, y, row)
			}
		}
	}
	drawCloud(img, f)
	return img
}

// lerpColor linearly interpolates between two colours.
func lerpColor(a, b color.RGBA, t float64) color.RGBA {
	l := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 0xff}
}

// pill draws a horizontal round-capped bar.
func pill(img *image.RGBA, x0, y, x1, h float64, col color.RGBA) {
	r := h / 2
	fillRect(img, x0, y, x1, y+h, col)
	disc(img, x0, y+r, r, col)
	disc(img, x1, y+r, r, col)
}

// drawCloud renders the golden nimbus onto a tile of side f: deep-gold bottom
// lobes first (their lower halves stay visible as the cel-shaded underside),
// then the light-gold body and top lobes over them. A small trailing puff sits
// behind (dropped at tiny sizes where it would only blur).
func drawCloud(img *image.RGBA, f float64) {
	cx, cy := f*0.52, f*0.54
	// Underside lobes (deep gold) — the lower scallops remain visible.
	disc(img, cx-f*0.205, cy+f*0.045, f*0.095, goldDeep)
	disc(img, cx-f*0.045, cy+f*0.065, f*0.105, goldDeep)
	disc(img, cx+f*0.115, cy+f*0.060, f*0.100, goldDeep)
	disc(img, cx+f*0.245, cy+f*0.030, f*0.080, goldDeep)
	// Body + top billows (light gold).
	fillRect(img, cx-f*0.205, cy-f*0.045, cx+f*0.245, cy+f*0.045, goldLight)
	disc(img, cx-f*0.205, cy, f*0.078, goldLight)
	disc(img, cx+f*0.245, cy, f*0.066, goldLight)
	disc(img, cx-f*0.125, cy-f*0.065, f*0.105, goldLight)
	disc(img, cx+f*0.020, cy-f*0.100, f*0.130, goldLight)
	disc(img, cx+f*0.165, cy-f*0.050, f*0.095, goldLight)
	if f >= 24 {
		// It FLIES: speed streaks + a trailing puff behind it.
		pill(img, cx-f*0.42, cy-f*0.075, cx-f*0.27, f*0.040, goldLight)
		pill(img, cx-f*0.46, cy+f*0.010, cx-f*0.33, f*0.040, goldLight)
		pill(img, cx-f*0.40, cy+f*0.095, cx-f*0.29, f*0.040, goldDeep)
	}
}

func inRoundRect(px, py, x0, y0, x1, y1, r float64) bool {
	if px < x0 || px > x1 || py < y0 || py > y1 {
		return false
	}
	// Clamp to the inner rectangle and measure corner distance.
	qx := math.Max(x0+r, math.Min(px, x1-r))
	qy := math.Max(y0+r, math.Min(py, y1-r))
	return math.Hypot(px-qx, py-qy) <= r
}

func disc(img *image.RGBA, cx, cy, r float64, col color.RGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy) <= r {
				img.Set(x, y, col)
			}
		}
	}
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 float64, col color.RGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if fx >= x0 && fx <= x1 && fy >= y0 && fy <= y1 {
				img.Set(x, y, col)
			}
		}
	}
}
