//go:build ignore

// chaticons_gen draws the chat page's app icons (chatui/*.png): a white chat
// bubble on the page's own blue (--mine in chat.css). The PNGs are committed
// and embedded; this only redraws them. Run from internal/remoteproxy:
//
//	go generate ./internal/remoteproxy   (or: go run chaticons_gen.go)
//
// The build tag keeps it out of every normal build and test run.
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

var (
	brand = color.NRGBA{0x1f, 0x4f, 0xd8, 0xff} // --mine
	white = color.NRGBA{0xff, 0xff, 0xff, 0xff}
)

// icon is one file: its size, whether the background fills the square (a
// maskable or Apple icon, which the platform crops) or is a rounded square
// with clear corners, and how large the glyph is (a maskable icon keeps it
// inside the central safe circle, 80% of the square).
type icon struct {
	name  string
	size  int
	bleed bool
	scale float64
}

func main() {
	for _, ic := range []icon{
		{"chatui/icon-192.png", 192, false, 1},
		{"chatui/icon-512.png", 512, false, 1},
		{"chatui/icon-maskable-512.png", 512, true, 0.8},
		{"chatui/apple-touch-icon.png", 180, true, 0.9},
	} {
		if err := write(ic); err != nil {
			panic(err)
		}
	}
}

func write(ic icon) error {
	img := image.NewNRGBA(image.Rect(0, 0, ic.size, ic.size))
	const ss = 4 // samples per axis per pixel
	for py := 0; py < ic.size; py++ {
		for px := 0; px < ic.size; px++ {
			var bg, fg int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(ic.size)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(ic.size)
					if !ic.bleed && !inRoundRect(x, y, 0, 0, 1, 1, 0.22) {
						continue
					}
					bg++
					// The glyph, scaled about the centre.
					gx, gy := 0.5+(x-0.5)/ic.scale, 0.5+(y-0.5)/ic.scale
					if glyph(gx, gy) {
						fg++
					}
				}
			}
			n := float64(ss * ss)
			a, f := float64(bg)/n, 0.0
			if bg > 0 {
				f = float64(fg) / float64(bg)
			}
			img.SetNRGBA(px, py, color.NRGBA{
				R: mix(brand.R, white.R, f), G: mix(brand.G, white.G, f), B: mix(brand.B, white.B, f),
				A: uint8(math.Round(a * 255)),
			})
		}
	}
	out, err := os.Create(ic.name)
	if err != nil {
		return err
	}
	defer out.Close()
	return (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(out, img)
}

// glyph reports whether (x, y) in the unit square is white: a bubble with a
// tail at the lower left, and three dots cut out of it.
func glyph(x, y float64) bool {
	body := inRoundRect(x, y, 0.18, 0.22, 0.82, 0.68, 0.14)
	tail := inTriangle(x, y, 0.28, 0.6, 0.46, 0.6, 0.22, 0.8)
	if !body && !tail {
		return false
	}
	for _, cx := range []float64{0.36, 0.5, 0.64} {
		if math.Hypot(x-cx, y-0.45) < 0.045 {
			return false
		}
	}
	return true
}

func inRoundRect(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Max(x0+r, math.Min(x, x1-r))
	cy := math.Max(y0+r, math.Min(y, y1-r))
	return math.Hypot(x-cx, y-cy) <= r
}

func inTriangle(x, y, ax, ay, bx, by, cx, cy float64) bool {
	side := func(px, py, qx, qy float64) float64 { return (qx-px)*(y-py) - (qy-py)*(x-px) }
	d1, d2, d3 := side(ax, ay, bx, by), side(bx, by, cx, cy), side(cx, cy, ax, ay)
	return !((d1 < 0 || d2 < 0 || d3 < 0) && (d1 > 0 || d2 > 0 || d3 > 0))
}

func mix(a, b uint8, f float64) uint8 {
	return uint8(math.Round(float64(a)*(1-f) + float64(b)*f))
}
