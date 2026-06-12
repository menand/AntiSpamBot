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
