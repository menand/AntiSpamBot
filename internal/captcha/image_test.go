package captcha

import (
	"bytes"
	"image/png"
	"math/rand/v2"
	"testing"
)

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
	// Every token in the emoji-mode pool must have a vendored glyph — catches
	// pool/assets drift when someone adds an emoji to emojiCategories without
	// its PNG. Circles are text-only and intentionally not covered here.
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
