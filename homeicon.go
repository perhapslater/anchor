package main

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

// Home-screen icons. A phone's home screen wants an opaque, square PNG: iOS
// ignores SVG and paints transparent pixels black, and most of Anchor's icons
// are transparent SVGs. So Anchor sets a PNG of the icon on a plate, the same
// colour as the plate behind icons on the dashboard tiles. Go's standard
// library is all it needs.

const (
	homeIconSize = 512
	homeIconBox  = homeIconSize * 62 / 100 // the icon fills the middle 62%, clear of rounded corners and masks
	maxIconSide  = 4096                    // refuse absurdly large PNGs before decoding them
)

var homePlate = color.RGBA{0xef, 0xeb, 0xe4, 0xff}

// fallbackHomeIcon is Anchor's own icon, sent when an app has no PNG to work from.
//
//go:embed web/brand/icon-maskable-512.png
var fallbackHomeIcon []byte

// homeIcon turns a PNG into a home-screen icon. It reports false if data
// isn't a PNG it can use.
func homeIcon(data []byte) ([]byte, bool) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width > maxIconSide || cfg.Height > maxIconSide {
		return nil, false
	}
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	sb := src.Bounds()
	if sb.Dx() < 1 || sb.Dy() < 1 {
		return nil, false
	}

	// Fit the icon inside the middle of the plate, keeping its shape.
	w, h := homeIconBox, homeIconBox
	if sb.Dx() > sb.Dy() {
		h = max(1, homeIconBox*sb.Dy()/sb.Dx())
	} else {
		w = max(1, homeIconBox*sb.Dx()/sb.Dy())
	}
	plate := image.NewRGBA(image.Rect(0, 0, homeIconSize, homeIconSize))
	draw.Draw(plate, plate.Bounds(), image.NewUniform(homePlate), image.Point{}, draw.Src)
	x, y := (homeIconSize-w)/2, (homeIconSize-h)/2
	draw.Draw(plate, image.Rect(x, y, x+w, y+h), resize(src, w, h), image.Point{}, draw.Over)

	var out bytes.Buffer
	if err := png.Encode(&out, plate); err != nil {
		return nil, false
	}
	return out.Bytes(), true
}

// tap is one input pixel's share of an output pixel.
type tap struct {
	i int
	w float32
}

// taps works out, for each of n output pixels, which of the m input pixels
// make it up and by how much. It's a tent filter that widens when shrinking,
// so a big icon squeezed small comes out smooth instead of jagged.
func taps(m, n int) [][]tap {
	scale := float64(m) / float64(n)
	support := math.Max(1, scale)
	out := make([][]tap, n)
	for o := range out {
		centre := (float64(o) + 0.5) * scale
		var sum float64
		for i := max(0, int(math.Floor(centre-support))); i <= min(m-1, int(math.Ceil(centre+support))); i++ {
			d := math.Abs(float64(i)+0.5-centre) / support
			if d >= 1 {
				continue
			}
			out[o] = append(out[o], tap{i, float32(1 - d)})
			sum += 1 - d
		}
		if len(out[o]) == 0 {
			out[o] = []tap{{min(m-1, int(centre)), 1}}
			continue
		}
		for k := range out[o] {
			out[o][k].w /= float32(sum)
		}
	}
	return out
}

// resize scales src to w x h. Colours are averaged premultiplied by alpha, so
// the soft edge of a see-through icon doesn't pick up a dark or light fringe.
func resize(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	cols, rows := taps(b.Dx(), w), taps(b.Dy(), h)

	// Across first: every source row becomes w pixels.
	mid := make([]float32, w*b.Dy()*4)
	for y := 0; y < b.Dy(); y++ {
		for x, ts := range cols {
			var acc [4]float32
			for _, t := range ts {
				r, g, bl, a := src.At(b.Min.X+t.i, b.Min.Y+y).RGBA()
				acc[0] += t.w * float32(r)
				acc[1] += t.w * float32(g)
				acc[2] += t.w * float32(bl)
				acc[3] += t.w * float32(a)
			}
			copy(mid[(y*w+x)*4:], acc[:])
		}
	}

	// Then down: the source rows become h pixels.
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y, ts := range rows {
		for x := 0; x < w; x++ {
			var acc [4]float32
			for _, t := range ts {
				m := mid[(t.i*w+x)*4:]
				acc[0] += t.w * m[0]
				acc[1] += t.w * m[1]
				acc[2] += t.w * m[2]
				acc[3] += t.w * m[3]
			}
			a := to8(acc[3])
			o := out.PixOffset(x, y)
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] =
				min(to8(acc[0]), a), min(to8(acc[1]), a), min(to8(acc[2]), a), a
		}
	}
	return out
}

// to8 turns a 16-bit channel value into 8 bits.
func to8(v float32) uint8 {
	return uint8(min(255, max(0, v/257+0.5)))
}
