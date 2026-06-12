# Image Captcha Mode + Bot Versioning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Третий режим капчи `image` — фото со слегка искажённым глифом правильной эмодзи (ответ уходит из текстового канала, парсинг-боты ломаются) + версия бота в `/info` и стартовом уведомлении.

**Architecture:** Глифы Noto Emoji PNG 128px вшиты через `go:embed`; рендер — чистый Go (`image/draw` + `golang.org/x/image` для аффинных трансформаций), CGO_ENABLED=0 сохраняется. Механика капчи (store, pending_captchas, callbacks, restore) не меняется — только генерация контента в `runCaptcha` (`SendPhoto` вместо `SendMessage`). Версия — через `-ldflags -X main.version`.

**Tech Stack:** Go 1.25+, telego v1.9, `golang.org/x/image` (новая зависимость), Noto Emoji PNG assets (Apache 2.0).

**Spec:** `docs/superpowers/specs/2026-06-12-image-captcha-design.md`

**Контекст кодовой базы для исполнителя:**
- Капча-флоу: `internal/bot/handlers.go::runCaptcha` — restrict → delay → `captcha.New(mode)` → отправка сообщения с inline-клавиатурой → `store.Put` + `db.PutPending` → `waitTimeout`.
- Режимы: `internal/captcha/captcha.go` — `Mode` (`circles`|`emoji`), `New(mode)` собирает `Challenge{Options []Token, CorrectIdx}`; `Token{Emoji, Prompt}`. Для emoji-режима — по одной эмодзи из каждой из 6 категорий `emojiCategories`.
- Per-chat режим: `chat_settings.captcha_mode` (TEXT, NULL=circles), резолв в `internal/bot/access.go::effectiveCaptchaMode`, UI в `internal/bot/menu.go` (`captchaModeRow`, кейс `cmode` в `handleMenuCallback`).
- Проект собирается `make build`, тестируется `go test -race ./...`, `gofmt -l .` должен быть пуст.
- Коммиты: без упоминаний Claude/AI, английский императив, push в origin/main после каждой задачи.

---

### Task 1: Noto Emoji assets

Скачать 43 глифа (все эмодзи из `emojiCategories`) + NOTICE. Без этих файлов `go:embed` в Task 2 не скомпилируется, поэтому ассеты идут первыми.

**Files:**
- Create: `internal/captcha/assets/emoji_u*.png` (43 файла)
- Create: `internal/captcha/assets/NOTICE.md`

- [ ] **Step 1: Скачать глифы**

```bash
mkdir -p internal/captcha/assets && cd internal/captcha/assets
for cp in 1f98a 1f981 1f43c 1f412 1f418 1f992 1f99d 1f984 \
          1f98b 1f41d 1f989 1f985 1f427 1f423 \
          1f422 1f419 1f41f 1f42c 1f40a 1f40d 1f438 \
          2600 1f319 2b50 1f308 2744 1f525 1f338 1f33b 1f344 \
          1f34e 1f34c 1f353 1f355 1f369 1f34b 1f349 \
          1f388 1f381 1f680 26bd 1f3b8 1f4da; do
  curl -fsSL -o "emoji_u${cp}.png" \
    "https://raw.githubusercontent.com/googlefonts/noto-emoji/main/png/128/emoji_u${cp}.png" || echo "FAILED: ${cp}"
done
cd ../../..
```

Кодпоинты соответствуют `emojiCategories` в `internal/captcha/captcha.go` (8 зверей, 6 летунов, 7 водно-ползучих, 9 природы, 7 еды, 6 вещей). У ☀️ (2600) и ❄️ (2744) вариационный селектор FE0F в имени файла отброшен — схема именования Noto.

- [ ] **Step 2: Проверить, что скачались все 43 и это PNG**

Run: `ls internal/captcha/assets/*.png | wc -l && file internal/captcha/assets/*.png | grep -cv "PNG image"`
Expected: `43` и `0`. Если меньше 43 или есть не-PNG — повторить недостающие (флаг `-fsSL` у curl скрывает HTML-ошибки, поэтому проверка обязательна).

- [ ] **Step 3: Создать NOTICE.md**

```markdown
# Noto Emoji assets

PNG glyphs in this directory are from the Noto Emoji project
(https://github.com/googlefonts/noto-emoji), © Google,
licensed under the Apache License, Version 2.0
(https://www.apache.org/licenses/LICENSE-2.0).

Only the glyphs used by the captcha emoji pool are vendored (128×128 px).
```

- [ ] **Step 4: Commit**

```bash
git add internal/captcha/assets && git commit -m "Vendor Noto Emoji glyphs for image captcha" && git push origin main
```

---

### Task 2: Glyph mapping + loading

**Files:**
- Create: `internal/captcha/image.go` (пока только маппинг и загрузка)
- Create: `internal/captcha/image_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/captcha/image_test.go`:

```go
package captcha

import "testing"

func TestGlyphFile(t *testing.T) {
	tests := []struct {
		emoji string
		want  string
	}{
		{"🦊", "emoji_u1f98a.png"},
		{"☀️", "emoji_u2600.png"}, // FE0F variation selector dropped
		{"⭐", "emoji_u2b50.png"},
		{"⚽", "emoji_u26bd.png"},
	}
	for _, tc := range tests {
		if got := glyphFile(tc.emoji); got != tc.want {
			t.Errorf("glyphFile(%q) = %q, want %q", tc.emoji, got, tc.want)
		}
	}
}

func TestAllEmojiGlyphsPresent(t *testing.T) {
	// Every token in the emoji pool must have a vendored glyph — catches
	// pool/assets drift when someone adds an emoji without its PNG.
	for ci, cat := range emojiCategories {
		for _, tok := range cat {
			if _, err := loadGlyph(tok.Emoji); err != nil {
				t.Errorf("category %d: %v", ci, err)
			}
		}
	}
}

func TestLoadGlyphUnknown(t *testing.T) {
	if _, err := loadGlyph("💀"); err == nil {
		t.Fatal("expected error for emoji without a vendored glyph")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/captcha/ -run 'TestGlyphFile|TestAllEmojiGlyphsPresent|TestLoadGlyphUnknown' -v`
Expected: COMPILE FAIL — `undefined: glyphFile`, `undefined: loadGlyph`.

- [ ] **Step 3: Write the implementation**

`internal/captcha/image.go`:

```go
package captcha

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/png"
	"strings"
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/captcha/ -run 'TestGlyphFile|TestAllEmojiGlyphsPresent|TestLoadGlyphUnknown' -v`
Expected: PASS ×3. Если `TestAllEmojiGlyphsPresent` падает — в Task 1 скачан не тот файл, сверить кодпоинт.

- [ ] **Step 5: Commit**

```bash
git add internal/captcha/image.go internal/captcha/image_test.go
git commit -m "Add glyph mapping and loading for image captcha" && git push origin main
```

---

### Task 3: Render pipeline

**Files:**
- Modify: `internal/captcha/image.go` (добавить рендер)
- Modify: `internal/captcha/image_test.go` (добавить тесты)
- Modify: `go.mod` (новая зависимость `golang.org/x/image`)

- [ ] **Step 1: Добавить зависимость**

Run: `go get golang.org/x/image@latest && go mod tidy`
Expected: в `go.mod` появляется `golang.org/x/image` в require.

- [ ] **Step 2: Write the failing tests**

Добавить в `internal/captcha/image_test.go`:

```go
import (
	"bytes"
	"image/png"
	"math/rand/v2"
	"testing"
)

func testRNG() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }

func TestRenderImageAllGlyphs(t *testing.T) {
	for _, cat := range emojiCategories {
		for _, tok := range cat {
			data, err := renderImage(tok, testRNG())
			if err != nil {
				t.Fatalf("%s: %v", tok.Emoji, err)
			}
			if len(data) < 2_000 || len(data) > 300_000 {
				t.Errorf("%s: suspicious PNG size %d bytes", tok.Emoji, len(data))
			}
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("%s: output is not decodable PNG: %v", tok.Emoji, err)
			}
			if img.Bounds().Dx() != imgW || img.Bounds().Dy() != imgH {
				t.Errorf("%s: got %v, want %dx%d", tok.Emoji, img.Bounds(), imgW, imgH)
			}
		}
	}
}

func TestRenderImageUnknownGlyph(t *testing.T) {
	if _, err := renderImage(Token{Emoji: "💀", Prompt: "череп"}, testRNG()); err == nil {
		t.Fatal("expected error, got nil")
	}
}
```

(импорты слить с существующими в файле)

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/captcha/ -run TestRenderImage -v`
Expected: COMPILE FAIL — `undefined: renderImage`, `undefined: imgW`.

- [ ] **Step 4: Write the renderer**

Добавить в `internal/captcha/image.go` (импорты дополнить: `image/color`, `image/draw`, `math`, `math/rand/v2`, `xdraw "golang.org/x/image/draw"`, `"golang.org/x/image/math/f64"`):

```go
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

	// Rotation grows the bounding box; budget 1.35× so the glyph never
	// clips at the canvas edge.
	eff := float64(src.Dx()) * scale * 1.35
	const margin = 12.0
	tx := margin + rng.Float64()*math.Max(1, float64(imgW)-eff-2*margin)
	ty := margin + rng.Float64()*math.Max(1, float64(imgH)-eff-2*margin)

	cx, cy := float64(src.Dx())/2, float64(src.Dy())/2
	cos, sin := math.Cos(angle), math.Sin(angle)
	s := scale
	// p' = T(target center) ∘ R(angle) ∘ S(s) ∘ T(-glyph center) · p
	m := f64.Aff3{
		s * cos, -s * sin, tx + s*cx - s*(cos*cx-sin*cy),
		s * sin, s * cos, ty + s*cy - s*(sin*cx+cos*cy),
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/captcha/ -v`
Expected: все тесты пакета PASS (43 рендера занимают доли секунды).

- [ ] **Step 6: Глазами посмотреть один рендер (sanity check)**

```bash
cat > /tmp/render_preview.go <<'EOF'
//go:build ignore

package main

import (
	"os"

	"github.com/menand/AntiSpamBot/internal/captcha"
)

func main() {
	ch := captcha.New(captcha.ModeEmoji)
	data, err := captcha.RenderImage(ch.Correct())
	if err != nil {
		panic(err)
	}
	_ = os.WriteFile("/tmp/captcha_preview.png", data, 0o644)
	println("correct:", ch.Correct().Emoji, "->", "/tmp/captcha_preview.png")
}
EOF
go run /tmp/render_preview.go && open /tmp/captcha_preview.png
```

Expected: глиф узнаваем человеком, искажения видимы, но не разрушительны. Это ручной шаг — если выглядит плохо, крутить константы (амплитуду волны, число линий), не двигаясь дальше.

- [ ] **Step 7: Commit**

```bash
git add internal/captcha/image.go internal/captcha/image_test.go go.mod go.sum
git commit -m "Add image captcha renderer (distorted Noto glyph on noisy background)" && git push origin main
```

---

### Task 4: ModeImage in captcha package

**Files:**
- Modify: `internal/captcha/captcha.go:14-19` (const block), `:99-106` (New)
- Modify: `internal/captcha/captcha_test.go` (добавить тест)

- [ ] **Step 1: Write the failing test**

Добавить в `internal/captcha/captcha_test.go`:

```go
func TestNewImageMode(t *testing.T) {
	ch := New(ModeImage)
	if len(ch.Options) != len(emojiCategories) {
		t.Fatalf("got %d options, want %d", len(ch.Options), len(emojiCategories))
	}
	if ch.CorrectIdx < 0 || ch.CorrectIdx >= len(ch.Options) {
		t.Fatalf("CorrectIdx out of range: %d", ch.CorrectIdx)
	}
	// Image mode must draw from the emoji pool (glyphs exist only for it).
	if _, err := loadGlyph(ch.Correct().Emoji); err != nil {
		t.Fatalf("correct option has no glyph: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/captcha/ -run TestNewImageMode -v`
Expected: COMPILE FAIL — `undefined: ModeImage`.

- [ ] **Step 3: Implement**

В `internal/captcha/captcha.go` заменить const-блок:

```go
const (
	ModeCircles Mode = "circles" // default: 6 colored circles
	ModeEmoji   Mode = "emoji"   // one emoji from each of 6 categories
	ModeImage   Mode = "image"   // emoji options + distorted glyph photo as the prompt
)
```

и `New`:

```go
// New builds a fresh captcha challenge for the given mode. Unknown/empty
// modes fall back to ModeCircles. ModeImage reuses the emoji pool — the
// difference is only in presentation (the bot renders the correct glyph as
// a distorted photo instead of naming it in text).
func New(mode Mode) Challenge {
	switch mode {
	case ModeEmoji, ModeImage:
		return newEmoji()
	default:
		return newCircles()
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/captcha/ -v`
Expected: PASS, включая существующие тесты New (проверить, что ни один не сломался).

- [ ] **Step 5: Commit**

```bash
git add internal/captcha/captcha.go internal/captcha/captcha_test.go
git commit -m "Add image captcha mode constant" && git push origin main
```

---

### Task 5: Bot integration (runCaptcha, settings UI, mode resolve)

**Files:**
- Modify: `internal/bot/handlers.go` — `runCaptcha` (блок формирования текста/отправки, после строк с `ch := captcha.New(...)`)
- Modify: `internal/bot/access.go:66-79` — `effectiveCaptchaMode`
- Modify: `internal/bot/menu.go` — `cmode`-валидация в `handleMenuCallback`, `captchaModeRow`, `captchaModeLabel`, helpText

- [ ] **Step 1: effectiveCaptchaMode принимает image**

В `internal/bot/access.go` заменить switch в `effectiveCaptchaMode`:

```go
	switch captcha.Mode(s.CaptchaMode.String) {
	case captcha.ModeEmoji:
		return captcha.ModeEmoji
	case captcha.ModeImage:
		return captcha.ModeImage
	default:
		return captcha.ModeCircles
	}
```

- [ ] **Step 2: runCaptcha — рендер, фоллбек, SendPhoto**

В `internal/bot/handlers.go::runCaptcha` сейчас есть блок (после расчёта `captchaTimeout`):

```go
	ch := captcha.New(b.effectiveCaptchaMode(ctx, chatID))
	captchaTimeout := b.effectiveCaptchaTimeout(ctx, chatID)

	correct := ch.Correct()
	text := fmt.Sprintf(
		"Привет, %s!\nДля защиты от спама выбери <b>%s</b> за %d секунд.",
		mentionHTML(user), correct.Prompt, int(captchaTimeout.Seconds()),
	)
```

Заменить его и блок отправки (`params := tu.Message(...)` … `msg, err := b.api.SendMessage(ctx, params)`) на:

```go
	mode := b.effectiveCaptchaMode(ctx, chatID)
	ch := captcha.New(mode)
	captchaTimeout := b.effectiveCaptchaTimeout(ctx, chatID)
	correct := ch.Correct()

	// Image mode: pre-render the photo. On any render failure fall back to
	// the text prompt — a captcha must always go out.
	var photo []byte
	if mode == captcha.ModeImage {
		var rerr error
		photo, rerr = captcha.RenderImage(correct)
		if rerr != nil {
			b.log.Warn("render image captcha, falling back to text",
				"err", rerr, "emoji", correct.Emoji)
		}
	}

	buttons := make([]telego.InlineKeyboardButton, 0, len(ch.Options))
	for i, c := range ch.Options {
		buttons = append(buttons,
			tu.InlineKeyboardButton(c.Emoji).
				WithCallbackData(fmt.Sprintf("cap:%d:%d", user.ID, i)))
	}
	kb := tu.InlineKeyboard(
		tu.InlineKeyboardRow(buttons...),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("✅ Впустить (для админов)").
				WithCallbackData(fmt.Sprintf("capok:%d", user.ID))),
	)

	var msg *telego.Message
	var err error
	if photo != nil {
		caption := fmt.Sprintf(
			"Привет, %s!\nДля защиты от спама выбери эмодзи, наиболее похожую на картинку, за %d секунд.",
			mentionHTML(user), int(captchaTimeout.Seconds()),
		)
		p := tu.Photo(tu.ID(chatID),
			tu.File(tu.NameReader(bytes.NewReader(photo), "captcha.png"))).
			WithCaption(caption).
			WithParseMode(telego.ModeHTML).
			WithReplyMarkup(kb)
		if threadID != 0 {
			p = p.WithMessageThreadID(threadID)
		}
		msg, err = b.api.SendPhoto(ctx, p)
	} else {
		text := fmt.Sprintf(
			"Привет, %s!\nДля защиты от спама выбери <b>%s</b> за %d секунд.",
			mentionHTML(user), correct.Prompt, int(captchaTimeout.Seconds()),
		)
		params := tu.Message(tu.ID(chatID), text).
			WithParseMode(telego.ModeHTML).
			WithReplyMarkup(kb)
		if threadID != 0 {
			params = params.WithMessageThreadID(threadID)
		}
		msg, err = b.api.SendMessage(ctx, params)
	}
	if err != nil {
		b.log.Error("send captcha", "err", err, "chat", chatID, "user", user.ID)
		_ = b.release(ctx, chatID, user.ID)
		return
	}
```

Существующая клавиатура в текущем коде уже содержит ряд «Впустить» — блок выше её просто включает; не задублировать. Дальше по функции (`expires`, `store.Put`, `PutPending`, `waitTimeout`) — без изменений. В импорты `handlers.go` добавить `"bytes"`.

- [ ] **Step 3: Меню — кнопка и валидация**

`internal/bot/menu.go`, кейс `cmode` в `handleMenuCallback` — заменить условие:

```go
		mode := parts[3]
		if mode != string(captcha.ModeCircles) && mode != string(captcha.ModeEmoji) &&
			mode != string(captcha.ModeImage) {
			return nil
		}
```

`captchaModeRow` — заменить срез опций:

```go
	opts := []struct {
		mode  captcha.Mode
		label string
	}{
		{captcha.ModeCircles, "🟢 Кружки"},
		{captcha.ModeEmoji, "🦋 Эмодзи"},
		{captcha.ModeImage, "🖼 Картинка"},
	}
```

`captchaModeLabel` — заменить целиком:

```go
func captchaModeLabel(m captcha.Mode) string {
	switch m {
	case captcha.ModeEmoji:
		return "Эмодзи"
	case captcha.ModeImage:
		return "Картинка"
	default:
		return "Кружки"
	}
}
```

`renderChatSettings` — в блоке резолва текущего режима заменить:

```go
	captchaMode := captcha.ModeCircles
	if s.CaptchaMode.Valid {
		switch captcha.Mode(s.CaptchaMode.String) {
		case captcha.ModeEmoji:
			captchaMode = captcha.ModeEmoji
		case captcha.ModeImage:
			captchaMode = captcha.ModeImage
		}
	}
```

(сейчас там однострочный if только на ModeEmoji)

В `helpText` заменить фразу «вид капчи (кружки/эмодзи)» на «вид капчи (кружки/эмодзи/картинка)».

- [ ] **Step 4: Build + full tests**

Run: `go build ./... && go test -race ./... && gofmt -l .`
Expected: build OK, все тесты PASS, gofmt пуст. Особое внимание: `SendPhoto`/`WithMessageThreadID` должны существовать в telego v1.9 — если компиляция падает на builder-методах, проверить `go doc github.com/mymmrac/telego SendPhotoParams | grep -i thread`.

- [ ] **Step 5: Commit**

```bash
git add internal/bot/handlers.go internal/bot/access.go internal/bot/menu.go
git commit -m "Wire image captcha mode into bot: SendPhoto flow, settings UI, fallback" && git push origin main
```

---

### Task 6: Bot versioning

**Files:**
- Modify: `cmd/bot/main.go` (var version + передача в bot.New)
- Modify: `internal/bot/bot.go` (`New` сигнатура, поле, стартовое уведомление)
- Modify: `internal/bot/info.go` (строка в /info)
- Modify: `Makefile` (VERSION + ldflags + docker-up)
- Modify: `Dockerfile` (ARG VERSION + ldflags)
- Modify: `docker-compose.yml` (build args)
- Modify: `README.md` (заметка про VERSION при деплое)

- [ ] **Step 1: main.go**

В `cmd/bot/main.go` после блока import добавить:

```go
// version is stamped at build time via -ldflags "-X main.version=...".
// "dev" means a plain `go run` / untagged build.
var version = "dev"
```

и заменить вызов конструктора:

```go
	b, err := bot.New(cfg, log, version)
```

- [ ] **Step 2: bot.go**

Сигнатура и поле (в `internal/bot/bot.go`):

```go
func New(cfg *config.Config, log *slog.Logger, version string) (*Bot, error) {
```

в структуру `Bot` рядом с `startedAt time.Time` добавить поле `version string`, в литерале конструктора — `version: version,`. Если `version` пустая — нормализовать в начале New: `if version == "" { version = "dev" }`.

Стартовое уведомление в `Run` — заменить:

```go
	b.notifyOwners(ctx, fmt.Sprintf(
		"🟢 <b>Бот запущен</b>\nUsername: @%s\nВерсия: <code>%s</code>\nВосстановлено капч: %d",
		b.Username(), b.version, restored))
```

- [ ] **Step 3: info.go**

В `handleInfoCommand` заменить формирование text:

```go
	text := fmt.Sprintf(
		"🤖 <b>Информация о боте</b>\n\n"+
			"Username: @%s\n"+
			"Версия: <code>%s</code>\n"+
			"Запущен: <code>%s</code>\n"+
			"Работает: <b>%s</b>",
		b.Username(),
		b.version,
		started,
		formatUptimeRU(uptime),
	)
```

- [ ] **Step 4: Makefile**

Заменить начало файла:

```makefile
.PHONY: build run test vet tidy docker-up docker-down docker-logs clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/bot ./cmd/bot
```

и цель docker-up:

```makefile
docker-up:
	VERSION=$(VERSION) docker compose up -d --build
```

- [ ] **Step 5: Dockerfile**

Заменить builder-часть:

```dockerfile
FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/bot ./cmd/bot
```

- [ ] **Step 6: docker-compose.yml**

Заменить `build: .` на:

```yaml
    build:
      context: .
      args:
        VERSION: ${VERSION:-dev}
```

- [ ] **Step 7: README — заметка про версию при деплое**

В секции деплоя (`### 3. Деплой на VDS через Docker` / auto-deploy) добавить после команды запуска:

```markdown
> Чтобы бот знал свою версию (`/info`, стартовое уведомление), собирай через
> `make docker-up` — он сам подставит `git describe`. Голый
> `docker compose up -d --build` покажет версию `dev`; в cron-скрипте деплоя
> используй `make docker-up` или `VERSION=$(git describe --tags --always) docker compose up -d --build`.
```

- [ ] **Step 8: Verify**

Run: `go build ./... && go run ./cmd/bot 2>&1 | head -2 || true` — бот без BOT_TOKEN упадёт с «BOT_TOKEN is not set», это ожидаемо (важно, что компилируется).
Run: `make build && ./bin/bot 2>&1 | head -2 || true` — то же, но бинарь собран с версией.
Run: `go test -race ./... && gofmt -l .`
Expected: тесты PASS, gofmt пуст.

- [ ] **Step 9: Commit**

```bash
git add cmd/bot/main.go internal/bot/bot.go internal/bot/info.go Makefile Dockerfile docker-compose.yml README.md
git commit -m "Add build-time version: /info line, startup notify, ldflags plumbing" && git push origin main
```

---

### Task 7: Docs + final pass

**Files:**
- Modify: `CLAUDE.md` (режимы капчи, новая зависимость)
- Modify: `README.md` (режим «Картинка» в фичах/«Как выглядит»)

- [ ] **Step 1: CLAUDE.md**

В секции «Captcha lifecycle» п.2 упомянуть: режимов три (`circles|emoji|image`); image = emoji-пул + фото искажённого глифа (`internal/captcha/image.go`, Noto PNG через go:embed, рендер чистый Go, ошибки рендера фоллбечатся в текстовый emoji-режим). В «When making changes» добавить пункт: «Добавляя эмодзи в пул — положи glyph PNG в `internal/captcha/assets/` (тест `TestAllEmojiGlyphsPresent` ловит рассинхрон)». Упомянуть версию: `-ldflags -X main.version`, деплой через `make docker-up`.

- [ ] **Step 2: README**

В «Фичи» в пункт про 2 режима капчи: «**3 режима капчи** — кружки, эмодзи или картинка (искажённый глиф: ответа нет в тексте — парсинг-боты ломаются, капча не требует знания языка)». В «Как выглядит» добавить короткий блок про режим «Картинка»:

```markdown
**Режим «Картинка»:**

> *(фото: искажённая эмодзи на шумном фоне)*
> **Привет, @vasya!**
> Для защиты от спама выбери эмодзи, наиболее похожую на картинку, за 30 секунд.
> [🍕] [🦋] [🌈] [⚽] [🦊] [🐢]
```

- [ ] **Step 3: Final verification**

Run: `go build ./... && go vet ./... && go test -race ./... && gofmt -l .`
Expected: всё чисто.

- [ ] **Step 4: Commit + tag**

```bash
git add CLAUDE.md README.md
git commit -m "Document image captcha mode and versioning" && git push origin main
git tag v1.0.0 && git push origin v1.0.0
```

Тег — по рекомендации спеки: дальше `git describe` даёт осмысленные версии (`v1.0.0-N-g<hash>`).

---

## Self-review checklist (выполнен при написании)

- Spec coverage: UX/настройки → Task 5; глифы/NOTICE → Task 1-2; искажения/x-image/фоллбек рендера → Task 3 + Task 5 Step 2; ModeImage → Task 4; интеграция → Task 5; версия (main/bot/info/Makefile/Dockerfile/compose/README/тег) → Task 6 + Task 7; тесты спеки → Task 2 (маппинг, unknown), Task 3 (все глифы, PNG, размер), Task 4 (режим), валидация cmode → Task 5.
- Types: `renderImage(Token, *rand.Rand) ([]byte, error)`, `RenderImage(Token) ([]byte, error)`, `glyphFile(string) string`, `loadGlyph(string) (image.Image, error)`, `bot.New(cfg, log, version)` — согласованы между задачами.
- Известный риск: точные имена builder-методов telego (`SendPhoto`, `WithMessageThreadID` у SendPhotoParams) — проверяются компиляцией в Task 5 Step 4 с командой диагностики.
