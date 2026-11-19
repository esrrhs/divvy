// genicon renders the divvy app icon: a dark rounded square with the brand
// blue and a white diagonal "division" bar — divide-and-conquer in one mark.
// Output is a 512px PNG (the source for iconutil's .icns).
package main

import (
	"flag"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
)

const size = 512

var (
	ink       = color.RGBA{0x17, 0x14, 0x28, 0xff}
	inkTop    = color.RGBA{0x24, 0x20, 0x3b, 0xff}
	plate     = color.RGBA{0x6a, 0x5a, 0xe8, 0xff}
	whiteMark = color.RGBA{0xee, 0xec, 0xff, 0xff}
)

func main() {
	out := flag.String("out", "icon_512.png", "output png path")
	flag.Parse()

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	// Vertical gradient background.
	for y := 0; y < size; y++ {
		t := float64(y) / float64(size-1)
		c := color.RGBA{
			uint8(float64(inkTop.R)*(1-t) + float64(ink.R)*t),
			uint8(float64(inkTop.G)*(1-t) + float64(ink.G)*t),
			uint8(float64(inkTop.B)*(1-t) + float64(ink.B)*t),
			0xff,
		}
		for x := 0; x < size; x++ {
			img.Set(x, y, c)
		}
	}

	clip := image.Rect(64, 64, size-64, size-64)
	const radius = 96

	// Brand plate.
	fillRounded(img, clip, radius, plate)

	// The divide bar: a thick parallelogram from the upper right to the
	// lower left, thin white core over a translucent white plate.
	barThickness := 46
	upper := []image.Point{{cx() + 170, cy() - 200}, {cx() + 250, cy() - 120}}
	lower := []image.Point{{cx() - 250, cy() + 120}, {cx() - 170, cy() + 200}}
	fillRoundedPoly(img, clip, radius, fade(plate, whiteMark, 0.55),
		[]image.Point{upper[0], upper[1], lower[0], lower[1]})
	// Thin white core, offset so the bar has depth.
	core := []image.Point{
		{cx() + 150, cy() - 180},
		{cx() + 196, cy() - 134},
		{cx() - 196, cy() + 134},
		{cx() - 150, cy() + 180},
	}
	fillRoundedPoly(img, clip, radius, whiteMark, core)
	_ = barThickness

	f, err := os.Create(*out)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

func cx() int { return size / 2 }
func cy() int { return size / 2 }

func fade(a, b color.RGBA, t float64) color.RGBA {
	return color.RGBA{
		uint8(float64(a.R)*(1-t) + float64(b.R)*t),
		uint8(float64(a.G)*(1-t) + float64(b.G)*t),
		uint8(float64(a.B)*(1-t) + float64(b.B)*t),
		0xff,
	}
}

func fillRounded(img draw.Image, r image.Rectangle, radius int, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if inRounded(x, y, r, radius) {
				img.Set(x, y, c)
			}
		}
	}
}

func fillRoundedPoly(img draw.Image, r image.Rectangle, radius int, c color.RGBA, poly []image.Point) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if inRounded(x, y, r, radius) && inPoly(x, y, poly) {
				img.Set(x, y, c)
			}
		}
	}
}

func inRounded(x, y int, r image.Rectangle, radius int) bool {
	cx, cy := r.Min.X+radius, r.Min.Y+radius
	if x >= cx && x < r.Max.X-radius {
		return y >= r.Min.Y && y < r.Max.Y
	}
	if y >= cy && y < r.Max.Y-radius {
		return x >= r.Min.X && x < r.Max.X
	}
	var ox, oy int
	switch {
	case x < cx && y < cy:
		ox, oy = cx, cy
	case x >= r.Max.X-radius && y < cy:
		ox, oy = r.Max.X-radius, cy
	case x < cx && y >= r.Max.Y-radius:
		ox, oy = cx, r.Max.Y-radius
	default:
		ox, oy = r.Max.X-radius, r.Max.Y-radius
	}
	dx, dy := x-ox, y-oy
	return dx*dx+dy*dy <= radius*radius
}

func inPoly(x, y int, pts []image.Point) bool {
	inside := false
	j := len(pts) - 1
	for i := 0; i < len(pts); i++ {
		pi, pj := pts[i], pts[j]
		if (pi.Y > y) != (pj.Y > y) &&
			x < (pj.X-pi.X)*(y-pi.Y)/(pj.Y-pi.Y)+pi.X {
			inside = !inside
		}
		j = i
	}
	return inside
}
