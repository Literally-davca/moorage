//go:build ignore

// Generates the notification logos docs/assets/notify/{hub,agent,watchdog}.png (256×256)
// from the geometry of web/public/favicon.svg (the Moorage anchor), one accent per role.
// Discord can't show SVG avatars, so the logos are PNG.
//
//	go run ./internal/notify/icons_gen.go
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const size = 256
const scale = size / 32.0 // the favicon viewBox is 32×32
const samples = 4         // supersampling per axis

type vec struct{ x, y float64 }

func dist(a, b vec) float64 { return math.Hypot(a.x-b.x, a.y-b.y) }

func segDist(p, a, b vec) float64 {
	ab := vec{b.x - a.x, b.y - a.y}
	t := ((p.x-a.x)*ab.x + (p.y-a.y)*ab.y) / (ab.x*ab.x + ab.y*ab.y)
	t = math.Max(0, math.Min(1, t))
	return dist(p, vec{a.x + t*ab.x, a.y + t*ab.y})
}

// inside the rounded square (rx 9)
func inRoundRect(p vec) bool {
	const r = 9.0
	cx := math.Max(r, math.Min(32-r, p.x))
	cy := math.Max(r, math.Min(32-r, p.y))
	return p.x >= 0 && p.y >= 0 && p.x <= 32 && p.y <= 32 && dist(p, vec{cx, cy}) <= r
}

// on the anchor stroke (width 2.2, round caps)
func onAnchor(p vec) bool {
	const w = 2.2 / 2
	if math.Abs(dist(p, vec{16, 8.5})-2.5) <= w { // ring
		return true
	}
	if segDist(p, vec{16, 11}, vec{16, 25}) <= w || segDist(p, vec{11, 15}, vec{21, 15}) <= w {
		return true
	}
	// lower half circle from (8,19) to (24,19), radius 8, centre (16,19)
	c := vec{16, 19}
	if p.y >= 19 && math.Abs(dist(p, c)-8) <= w {
		return true
	}
	return dist(p, vec{8, 19}) <= w || dist(p, vec{24, 19}) <= w
}

func render(bg color.NRGBA, fg color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var inBG, inFG int
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					p := vec{(float64(x) + (float64(sx)+0.5)/samples) / scale, (float64(y) + (float64(sy)+0.5)/samples) / scale}
					if inRoundRect(p) {
						inBG++
						if onAnchor(p) {
							inFG++
						}
					}
				}
			}
			if inBG == 0 {
				continue
			}
			n := float64(samples * samples)
			f := float64(inFG) / float64(inBG)
			mix := func(a, b uint8) uint8 { return uint8(math.Round(float64(a)*(1-f) + float64(b)*f)) }
			img.SetNRGBA(x, y, color.NRGBA{mix(bg.R, fg.R), mix(bg.G, fg.G), mix(bg.B, fg.B), uint8(math.Round(255 * float64(inBG) / n))})
		}
	}
	return img
}

func main() {
	dir := filepath.Join("docs", "assets", "notify")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	ink := color.NRGBA{0x2A, 0x12, 0x06, 0xFF}
	for name, bg := range map[string]color.NRGBA{
		"hub":      {0xFF, 0x7A, 0x45, 0xFF}, // brand orange
		"agent":    {0x5B, 0x9B, 0xFF, 0xFF}, // blue
		"watchdog": {0xF2, 0xB5, 0x44, 0xFF}, // amber
	} {
		f, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			panic(err)
		}
		if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(f, render(bg, ink)); err != nil {
			panic(err)
		}
		f.Close()
	}
}
