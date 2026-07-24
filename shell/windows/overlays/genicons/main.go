// Command genicons generates the three overlay .ico files (synced / syncing /
// warning) used by the Explorer shell extension. Run from this directory:
//
//	go run .   # writes ../icons/{ok,sync,warn}.ico
//
// Icons are committed; re-run only to change the artwork.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

var (
	green = color.RGBA{0x2e, 0x7d, 0x32, 0xff} // synced: the universal green check
	// Nimbo indigo (the app's brand colour) for the in-progress badge, instead
	// of the Nextcloud-ish blue.
	indigo = color.RGBA{0x58, 0x56, 0xE0, 0xff}
	amber  = color.RGBA{0xf0, 0xa0, 0x20, 0xff}
	white  = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

func main() {
	out := filepath.Join("..", "icons")
	_ = os.MkdirAll(out, 0o755)
	write(filepath.Join(out, "ok.ico"), func(s int) image.Image { return badge(s, green, glyphCheck) })
	write(filepath.Join(out, "sync.ico"), func(s int) image.Image { return badge(s, indigo, glyphSync) })
	write(filepath.Join(out, "warn.ico"), func(s int) image.Image { return badge(s, amber, glyphBang) })
}

type glyphFn func(img *image.RGBA, cx, cy, r float64, col color.RGBA)

// write builds a multi-size .ico (16 and 32 px, PNG-encoded entries).
func write(path string, make func(size int) image.Image) {
	sizes := []int{16, 32}
	var pngs [][]byte
	for _, s := range sizes {
		var b bytes.Buffer
		if err := png.Encode(&b, make(s)); err != nil {
			panic(err)
		}
		pngs = append(pngs, b.Bytes())
	}
	var buf bytes.Buffer
	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&buf, binary.LittleEndian, uint16(len(sizes)))
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		buf.WriteByte(byte(s)) // width
		buf.WriteByte(byte(s)) // height
		buf.WriteByte(0)       // palette
		buf.WriteByte(0)       // reserved
		binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
		binary.Write(&buf, binary.LittleEndian, uint16(32)) // bitcount
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

// badge draws a small colored disc with a white glyph in the LOWER-LEFT corner,
// leaving the rest transparent. Explorer composites this over the file/folder
// icon, so a corner badge (rather than a full-size disc) keeps the underlying
// icon visible.
func badge(size int, c color.RGBA, glyph glyphFn) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	f := float64(size)
	r := f * 0.30           // badge radius (~60% diameter)
	m := f * 0.04           // margin from the edges
	cx, cy := r+m, f-r-m    // lower-left
	// Anti-aliased disc with a thin white outline for contrast on any icon.
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			switch {
			case d <= r-1.2:
				img.Set(x, y, c)
			case d <= r-0.2:
				img.Set(x, y, white) // outline ring
			case d <= r+0.3:
				img.Set(x, y, blend(white, r+0.3-d))
			}
		}
	}
	glyph(img, cx, cy, r, white)
	return img
}

func blend(c color.RGBA, a float64) color.RGBA {
	if a < 0 {
		a = 0
	}
	if a > 1 {
		a = 1
	}
	return color.RGBA{c.R, c.G, c.B, uint8(a * 255)}
}

// glyphCheck draws a white check mark within the badge disc (cx,cy,r).
func glyphCheck(img *image.RGBA, cx, cy, r float64, col color.RGBA) {
	w := r * 0.30
	thickLine(img, cx-0.45*r, cy+0.02*r, cx-0.12*r, cy+0.38*r, w, col)
	thickLine(img, cx-0.12*r, cy+0.38*r, cx+0.50*r, cy-0.42*r, w, col)
}

// glyphBang draws a white exclamation mark within the badge disc.
func glyphBang(img *image.RGBA, cx, cy, r float64, col color.RGBA) {
	thickLine(img, cx, cy-0.48*r, cx, cy+0.15*r, r*0.26, col)
	disc(img, cx, cy+0.46*r, r*0.16, col)
}

// glyphSync draws a white circular arrow within the badge disc.
func glyphSync(img *image.RGBA, cx, cy, r float64, col color.RGBA) {
	rr := r * 0.52
	w := r * 0.22
	for deg := 110.0; deg <= 410.0; deg += 4 {
		t := deg * math.Pi / 180
		disc(img, cx+rr*math.Cos(t), cy+rr*math.Sin(t), w/2, col)
	}
	// Arrowhead near the gap.
	disc(img, cx+rr*math.Cos(110*math.Pi/180), cy+rr*math.Sin(110*math.Pi/180), r*0.20, col)
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

// thickLine draws a rounded line of width w from (x0,y0) to (x1,y1).
func thickLine(img *image.RGBA, x0, y0, x1, y1, w float64, col color.RGBA) {
	b := img.Bounds()
	half := w / 2
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if distToSeg(float64(x)+0.5, float64(y)+0.5, x0, y0, x1, y1) <= half {
				img.Set(x, y, col)
			}
		}
	}
}

func distToSeg(px, py, x0, y0, x1, y1 float64) float64 {
	dx, dy := x1-x0, y1-y0
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(px-x0, py-y0)
	}
	t := ((px-x0)*dx + (py-y0)*dy) / l2
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(x0+t*dx), py-(y0+t*dy))
}
