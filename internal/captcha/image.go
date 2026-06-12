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
