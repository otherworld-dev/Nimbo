package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Nimbo palette (matches the app icon's golden-nimbus branding).
var (
	trayIndigo = color.RGBA{0x58, 0x56, 0xE0, 0xff}
	trayGoldL  = color.RGBA{0xFF, 0xD5, 0x5F, 0xff} // nimbus body
	trayGoldD  = color.RGBA{0xEF, 0xA5, 0x3A, 0xff} // nimbus underside
	trayRed    = color.RGBA{0xd6, 0x39, 0x39, 0xff}
	trayGray   = color.RGBA{0x88, 0x90, 0x96, 0xff}
	trayWhite  = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

// trayIcon renders the system-tray icon for a sync state ("idle", "sync",
// "paused", "error") as PNG bytes: the Nimbo tile (indigo rounded square with
// the white swoosh cloud) plus a small state badge bottom-right — spinner while
// syncing (frame rotates it), pause bars, or a red !. badge draws a red dot
// top-right (pending notifications). Wails accepts PNG tray icons on Windows.
func trayIcon(state string, frame int, badge bool) []byte {
	const sz = 32
	img := image.NewRGBA(image.Rect(0, 0, sz, sz))
	f := float64(sz)

	// Branded tile: rounded square + swoosh cloud (greyed out while paused).
	tile := trayIndigo
	if state == "paused" {
		tile = trayGray
	}
	rad := f * 0.24
	for y := 0; y < sz; y++ {
		for x := 0; x < sz; x++ {
			if inRoundRectTray(float64(x)+0.5, float64(y)+0.5, 1, 1, f-1, f-1, rad) {
				img.Set(x, y, tile)
			}
		}
	}
	// Golden nimbus, slightly high-left so the state badge has room. Underside
	// lobes (deep gold) first; light body + top billows over them; one streak.
	ccx, ccy := f*0.52, f*0.48
	disc(img, ccx-f*0.205, ccy+f*0.055, f*0.105, trayGoldD)
	disc(img, ccx-f*0.030, ccy+f*0.075, f*0.115, trayGoldD)
	disc(img, ccx+f*0.150, ccy+f*0.060, f*0.105, trayGoldD)
	fillRect(img, ccx-f*0.205, ccy-f*0.05, ccx+f*0.245, ccy+f*0.05, trayGoldL)
	disc(img, ccx-f*0.205, ccy, f*0.085, trayGoldL)
	disc(img, ccx+f*0.245, ccy, f*0.072, trayGoldL)
	disc(img, ccx-f*0.115, ccy-f*0.075, f*0.115, trayGoldL)
	disc(img, ccx+f*0.040, ccy-f*0.110, f*0.140, trayGoldL)
	disc(img, ccx+f*0.180, ccy-f*0.055, f*0.100, trayGoldL)
	pillTray(img, ccx-f*0.45, ccy-f*0.02, ccx-f*0.34, f*0.06, trayGoldL)

	// State badge bottom-right.
	bx, by, br := f*0.74, f*0.74, f*0.235
	switch state {
	case "sync":
		disc(img, bx, by, br+1.2, trayWhite)
		// Rotating open arc with a leading arrowhead.
		rr := br * 0.62
		start := float64((frame*12)%360) * math.Pi / 180
		for a := 0.0; a <= 250; a += 6 {
			t := start + a*math.Pi/180
			disc(img, bx+rr*math.Cos(t), by+rr*math.Sin(t), br*0.16, trayIndigo)
		}
		end := start + 250*math.Pi/180
		disc(img, bx+rr*math.Cos(end), by+rr*math.Sin(end), br*0.30, trayIndigo)
	case "paused":
		disc(img, bx, by, br+1.2, trayWhite)
		fillRect(img, bx-br*0.42, by-br*0.5, bx-br*0.10, by+br*0.5, trayGray)
		fillRect(img, bx+br*0.10, by-br*0.5, bx+br*0.42, by+br*0.5, trayGray)
	case "error":
		disc(img, bx, by, br+1.2, trayWhite)
		disc(img, bx, by, br, trayRed)
		thick(img, bx, by-br*0.5, bx, by+br*0.12, br*0.26, trayWhite)
		disc(img, bx, by+br*0.48, br*0.14, trayWhite)
	}

	// Pending-notifications badge: a red dot with a white ring, top-right.
	if badge {
		nx, ny, nr := f*0.78, f*0.22, f*0.17
		disc(img, nx, ny, nr+1.4, trayWhite)
		disc(img, nx, ny, nr, trayRed)
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// inRoundRectTray reports whether a point is inside a rounded rectangle.
func inRoundRectTray(px, py, x0, y0, x1, y1, r float64) bool {
	if px < x0 || px > x1 || py < y0 || py > y1 {
		return false
	}
	qx := math.Max(x0+r, math.Min(px, x1-r))
	qy := math.Max(y0+r, math.Min(py, y1-r))
	return math.Hypot(px-qx, py-qy) <= r
}

// pillTray draws a horizontal round-capped bar.
func pillTray(img *image.RGBA, x0, y, x1, h float64, col color.RGBA) {
	r := h / 2
	fillRect(img, x0, y, x1, y+h, col)
	disc(img, x0, y+r, r, col)
	disc(img, x1, y+r, r, col)
}

func disc(img *image.RGBA, cx, cy, r float64, col color.RGBA) {
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			if math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy) <= r {
				img.Set(x, y, col)
			}
		}
	}
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 float64, col color.RGBA) {
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if fx >= x0 && fx <= x1 && fy >= y0 && fy <= y1 {
				img.Set(x, y, col)
			}
		}
	}
}

func thick(img *image.RGBA, x0, y0, x1, y1, w float64, col color.RGBA) {
	half := w / 2
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			if distSeg(float64(x)+0.5, float64(y)+0.5, x0, y0, x1, y1) <= half {
				img.Set(x, y, col)
			}
		}
	}
}

func distSeg(px, py, x0, y0, x1, y1 float64) float64 {
	dx, dy := x1-x0, y1-y0
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(px-x0, py-y0)
	}
	t := math.Max(0, math.Min(1, ((px-x0)*dx+(py-y0)*dy)/l2))
	return math.Hypot(px-(x0+t*dx), py-(y0+t*dy))
}
