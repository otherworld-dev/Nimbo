package main

import (
	"image/color"
	"testing"
)

var stockPalette = palette{
	tileTop:    color.RGBA{0x6F, 0x6A, 0xF2, 0xff},
	tileBot:    color.RGBA{0x4A, 0x3E, 0xD2, 0xff},
	cloudLight: color.RGBA{0xFF, 0xD5, 0x5F, 0xff},
	cloudDeep:  color.RGBA{0xEF, 0xA5, 0x3A, 0xff},
}

// The stock accent — in any case, with or without '#' — must reproduce the exact
// hand-tuned icon so Nimbo's own artwork never shifts.
func TestStockPaletteIsCanonical(t *testing.T) {
	for _, a := range []string{"#5856E0", "5856e0", " #5856e0 "} {
		if got := newPalette(a); got != stockPalette {
			t.Errorf("newPalette(%q) = %+v, want canonical stock palette", a, got)
		}
	}
}

func TestDerivedPaletteDiffersAndTracksAccent(t *testing.T) {
	p := newPalette("#CC2233") // a red accent
	if p == stockPalette {
		t.Fatal("a non-stock accent should derive a different palette")
	}
	if h, _, _ := rgbToHSL(p.tileTop); !(h < 25 || h > 335) {
		t.Errorf("derived tile hue %.0f should track the red accent", h)
	}
}

func TestParseHex(t *testing.T) {
	if got := parseHex("#a1b2c3"); got != (color.RGBA{0xA1, 0xB2, 0xC3, 0xff}) {
		t.Errorf("parseHex(#a1b2c3) = %+v", got)
	}
	// Malformed input must not panic; it falls back to stock indigo.
	if got := parseHex("not-a-colour"); got != (color.RGBA{0x58, 0x56, 0xE0, 0xff}) {
		t.Errorf("parseHex(garbage) = %+v, want stock indigo fallback", got)
	}
}

func TestHSLRoundTrip(t *testing.T) {
	for _, c := range []color.RGBA{
		{0x58, 0x56, 0xE0, 0xff}, {0xCC, 0x22, 0x33, 0xff},
		{0x20, 0xA0, 0x60, 0xff}, {0x80, 0x80, 0x80, 0xff}, {0xFF, 0xD5, 0x5F, 0xff},
	} {
		h, s, l := rgbToHSL(c)
		got := hslToRGB(h, s, l)
		if d(got.R, c.R) > 1 || d(got.G, c.G) > 1 || d(got.B, c.B) > 1 {
			t.Errorf("HSL round-trip %+v -> %+v", c, got)
		}
	}
}

func d(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}
