package captcha

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand/v2"
	"strings"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
)

// Vendored Noto Emoji glyphs (see assets/NOTICE.md). Embedded so the binary
// stays self-contained and CGO-free — color emoji cannot be rasterized from
// fonts in pure Go (COLR/CBDT tables are unsupported).
//
//go:embed assets/*.png
var glyphFS embed.FS

// glyphFile maps an emoji to its Noto asset name: codepoints in lowercase
// hex joined by '_', variation selectors (U+FE0F) dropped — Noto's own
// file-naming scheme.
func glyphFile(emoji string) string {
	var parts []string
	for _, r := range emoji {
		if r == 0xFE0F {
			continue
		}
		parts = append(parts, fmt.Sprintf("%x", r))
	}
	return "emoji_u" + strings.Join(parts, "_") + ".png"
}

// loadGlyph reads and decodes the Noto PNG for the given emoji from the
// embedded FS.
func loadGlyph(emoji string) (image.Image, error) {
	data, err := glyphFS.ReadFile("assets/" + glyphFile(emoji))
	if err != nil {
		return nil, fmt.Errorf("glyph for %q: %w", emoji, err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode glyph %q: %w", emoji, err)
	}
	return img, nil
}

// Canvas size. Big enough that the glyph survives Telegram's photo
// re-compression, small enough to render in microseconds.
const (
	imgW = 384
	imgH = 256
)

// RenderImage draws tok's glyph, mildly distorted, on a noisy light
// background and returns the PNG bytes. Distortions are deliberately mild:
// the goal is to keep the answer out of the text channel (kills text-parsing
// bots), not to resist vision models — harsher noise hurts humans first.
func RenderImage(tok Token) ([]byte, error) {
	return renderImage(tok, rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())))
}

func renderImage(tok Token, rng *rand.Rand) ([]byte, error) {
	glyph, err := loadGlyph(tok.Emoji)
	if err != nil {
		return nil, err
	}

	canvas := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
	drawBackground(canvas, rng)
	drawGlyph(canvas, glyph, rng)
	canvas = applyWave(canvas, rng)
	drawLines(canvas, rng)
	addNoise(canvas, rng)

	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		return nil, fmt.Errorf("encode captcha png: %w", err)
	}
	return buf.Bytes(), nil
}

// drawBackground fills the canvas with a light vertical gradient plus a few
// translucent light blobs. Light tones only — the glyph must stay readable.
func drawBackground(img *image.RGBA, rng *rand.Rand) {
	light := func() float64 { return float64(215 + rng.IntN(31)) } // 215..245
	tr, tg, tb := light(), light(), light()
	br, bg, bb := light(), light(), light()
	for y := 0; y < imgH; y++ {
		t := float64(y) / float64(imgH)
		c := color.RGBA{
			R: uint8(tr*(1-t) + br*t),
			G: uint8(tg*(1-t) + bg*t),
			B: uint8(tb*(1-t) + bb*t),
			A: 255,
		}
		for x := 0; x < imgW; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	for i := 0; i < 3+rng.IntN(3); i++ {
		cx, cy := rng.IntN(imgW), rng.IntN(imgH)
		r := 40 + rng.IntN(50)
		shade := uint8(200 + rng.IntN(40))
		fillCircle(img, cx, cy, r, color.NRGBA{R: shade, G: shade, B: shade, A: 60})
	}
}

// drawGlyph paints the glyph with random scale (0.8–1.2), rotation (±25°)
// and position, via a CatmullRom-filtered affine transform.
func drawGlyph(canvas *image.RGBA, glyph image.Image, rng *rand.Rand) {
	src := glyph.Bounds()
	scale := 0.8 + rng.Float64()*0.4
	angle := (rng.Float64()*50 - 25) * math.Pi / 180

	// Rotation grows the bounding box; budget 1.35× and place the glyph
	// CENTER so the rotated bbox keeps `margin` clearance on every side.
	eff := float64(src.Dx()) * scale * 1.35
	const margin = 12.0
	half := eff / 2
	cxDst := margin + half + rng.Float64()*math.Max(1, float64(imgW)-eff-2*margin)
	cyDst := margin + half + rng.Float64()*math.Max(1, float64(imgH)-eff-2*margin)

	cx, cy := float64(src.Dx())/2, float64(src.Dy())/2
	cos, sin := math.Cos(angle), math.Sin(angle)
	s := scale
	// p' = T(dst center) ∘ R(angle) ∘ S(s) ∘ T(-glyph center) · p
	m := f64.Aff3{
		s * cos, -s * sin, cxDst - s*(cos*cx-sin*cy),
		s * sin, s * cos, cyDst - s*(sin*cx+cos*cy),
	}
	xdraw.CatmullRom.Transform(canvas, m, glyph, src, xdraw.Over, nil)
}

// applyWave shifts each row horizontally along a sine — cheap geometric
// distortion that breaks naive template matching.
func applyWave(img *image.RGBA, rng *rand.Rand) *image.RGBA {
	amp := 3 + rng.Float64()*3      // 3..6 px
	period := 40 + rng.Float64()*40 // 40..80 px
	phase := rng.Float64() * 2 * math.Pi
	out := image.NewRGBA(img.Bounds())
	for y := 0; y < imgH; y++ {
		dx := int(math.Round(amp * math.Sin(2*math.Pi*float64(y)/period+phase)))
		for x := 0; x < imgW; x++ {
			sx := x + dx
			if sx < 0 {
				sx = 0
			} else if sx >= imgW {
				sx = imgW - 1
			}
			out.SetRGBA(x, y, img.RGBAAt(sx, y))
		}
	}
	return out
}

// drawLines crosses the image with 2–4 translucent dark strokes.
func drawLines(img *image.RGBA, rng *rand.Rand) {
	for i := 0; i < 2+rng.IntN(3); i++ {
		x1, y1 := rng.Float64()*imgW, rng.Float64()*imgH
		x2, y2 := rng.Float64()*imgW, rng.Float64()*imgH
		shade := uint8(60 + rng.IntN(80))
		c := color.NRGBA{R: shade, G: shade, B: shade, A: 140}
		radius := 1 + rng.IntN(2) // stroke thickness 2..4 px
		steps := int(math.Hypot(x2-x1, y2-y1)) + 1
		for s := 0; s <= steps; s++ {
			t := float64(s) / float64(steps)
			fillCircle(img, int(x1+(x2-x1)*t), int(y1+(y2-y1)*t), radius, c)
		}
	}
}

// addNoise flips ~2% of pixels to random grays.
func addNoise(img *image.RGBA, rng *rand.Rand) {
	for i := 0; i < imgW*imgH/50; i++ {
		x, y := rng.IntN(imgW), rng.IntN(imgH)
		v := uint8(rng.IntN(256))
		img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
	}
}

// fillCircle alpha-blends a filled disc onto the (opaque) canvas.
func fillCircle(img *image.RGBA, cx, cy, r int, c color.NRGBA) {
	a := float64(c.A) / 255
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy > r*r {
				continue
			}
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= imgW || y >= imgH {
				continue
			}
			base := img.RGBAAt(x, y)
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(float64(c.R)*a + float64(base.R)*(1-a)),
				G: uint8(float64(c.G)*a + float64(base.G)*(1-a)),
				B: uint8(float64(c.B)*a + float64(base.B)*(1-a)),
				A: 255,
			})
		}
	}
}
