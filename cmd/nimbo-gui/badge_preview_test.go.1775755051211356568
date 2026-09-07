package main

import (
	"image/png"
	"os"
	"testing"
)

// TestBadgePreview renders a badged app icon from a source PNG for visual
// inspection. Runs only when NIMBO_BADGE_SRC is set (a dev/QA aid, not CI):
//
//	NIMBO_BADGE_SRC=in.png NIMBO_BADGE_OUT=out.png go test -run TestBadgePreview ./cmd/nimbo-gui/
// TestIcoPreview writes a full badged .ico via the production encoder for
// shell-decode QA. Same env gating as TestBadgePreview; OUT must end in .ico.
func TestIcoPreview(t *testing.T) {
	src := os.Getenv("NIMBO_ICO_SRC")
	out := os.Getenv("NIMBO_ICO_OUT")
	if src == "" || out == "" {
		t.Skip("set NIMBO_ICO_SRC and NIMBO_ICO_OUT to render an ico")
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeIcoFromImage(out, badgeAppIcon(img)); err != nil {
		t.Fatal(err)
	}
}

func TestBadgePreview(t *testing.T) {
	src := os.Getenv("NIMBO_BADGE_SRC")
	out := os.Getenv("NIMBO_BADGE_OUT")
	if src == "" || out == "" {
		t.Skip("set NIMBO_BADGE_SRC and NIMBO_BADGE_OUT to render a preview")
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	badged := badgeAppIcon(img)
	if badged == img {
		t.Fatal("badge was not applied (brand badge failed to decode?)")
	}
	o, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := png.Encode(o, badged); err != nil {
		t.Fatal(err)
	}
}
