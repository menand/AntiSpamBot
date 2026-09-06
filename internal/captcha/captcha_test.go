package captcha

import "testing"

func TestCirclesProducesAllSixDistinct(t *testing.T) {
	c := New(ModeCircles)
	if len(c.Options) != len(circles) {
		t.Fatalf("got %d options, want %d", len(c.Options), len(circles))
	}
	seen := make(map[string]struct{}, len(c.Options))
	for _, opt := range c.Options {
		if _, dup := seen[opt.Emoji]; dup {
			t.Fatalf("duplicate emoji %q in options", opt.Emoji)
		}
		seen[opt.Emoji] = struct{}{}
	}
}

func TestCirclesCorrectIdxInRange(t *testing.T) {
	for range 100 {
		c := New(ModeCircles)
		if c.CorrectIdx < 0 || c.CorrectIdx >= len(c.Options) {
			t.Fatalf("CorrectIdx %d out of range (len=%d)", c.CorrectIdx, len(c.Options))
		}
		if c.Correct() != c.Options[c.CorrectIdx] {
			t.Fatal("Correct() disagrees with Options[CorrectIdx]")
		}
	}
}

func TestCirclesShufflePermutesOrder(t *testing.T) {
	var differs bool
	for range 100 {
		c := New(ModeCircles)
		for j, opt := range c.Options {
			if opt.Emoji != circles[j].Emoji {
				differs = true
				break
			}
		}
		if differs {
			break
		}
	}
	if !differs {
		t.Fatal("100 shuffles never permuted order — shuffle is broken")
	}
}

func TestEmojiPicksOneFromEachCategory(t *testing.T) {
	// Строим обратный индекс: emoji → id категории.
	emojiToCat := make(map[string]int, 64)
	for catIdx, cat := range emojiCategories {
		for _, tok := range cat {
			emojiToCat[tok.Emoji] = catIdx
		}
	}

	// Гоняем много итераций; каждый челлендж обязан покрыть каждую категорию ровно один раз.
	const iterations = 200
	categoryHit := make([]int, len(emojiCategories)) // суммарные счётчики по всем прогонам
	for i := range iterations {
		c := New(ModeEmoji)
		if len(c.Options) != len(emojiCategories) {
			t.Fatalf("iter %d: got %d options, want %d", i, len(c.Options), len(emojiCategories))
		}
		seenCats := make(map[int]bool, len(emojiCategories))
		for _, opt := range c.Options {
			catIdx, ok := emojiToCat[opt.Emoji]
			if !ok {
				t.Fatalf("iter %d: emoji %q not in any category", i, opt.Emoji)
			}
			if seenCats[catIdx] {
				t.Fatalf("iter %d: category %d appeared twice", i, catIdx)
			}
			seenCats[catIdx] = true
			categoryHit[catIdx]++
		}
		if len(seenCats) != len(emojiCategories) {
			t.Fatalf("iter %d: only %d/%d categories present", i, len(seenCats), len(emojiCategories))
		}
		if c.CorrectIdx < 0 || c.CorrectIdx >= len(c.Options) {
			t.Fatalf("iter %d: CorrectIdx %d out of range", i, c.CorrectIdx)
		}
	}

	// Sanity: за 200 итераций каждая категория должна была выпасть много раз.
	for i, n := range categoryHit {
		if n != iterations {
			t.Errorf("category %d hit %d times, expected %d (one per iteration)", i, n, iterations)
		}
	}
}

func TestEmojiShufflesCategoryOrder(t *testing.T) {
	// После перемешивания emoji в слоте 0 от прогона к прогону должен
	// приходить из разных категорий. Если бы слот 0 всегда доставался
	// категории 0, шафл был бы сломан.
	emojiToCat := make(map[string]int, 64)
	for catIdx, cat := range emojiCategories {
		for _, tok := range cat {
			emojiToCat[tok.Emoji] = catIdx
		}
	}
	distinctFirstCats := make(map[int]struct{})
	for range 200 {
		c := New(ModeEmoji)
		distinctFirstCats[emojiToCat[c.Options[0].Emoji]] = struct{}{}
		if len(distinctFirstCats) >= 3 {
			return
		}
	}
	t.Fatalf("across 200 runs, slot 0 only came from %d distinct categories; shuffle is broken",
		len(distinctFirstCats))
}

func TestNewImageMode(t *testing.T) {
	ch := New(ModeImage)
	if len(ch.Options) != len(emojiCategories) {
		t.Fatalf("got %d options, want %d", len(ch.Options), len(emojiCategories))
	}
	if ch.CorrectIdx < 0 || ch.CorrectIdx >= len(ch.Options) {
		t.Fatalf("CorrectIdx out of range: %d", ch.CorrectIdx)
	}
	// Режим image обязан выбирать из emoji-пула (глифы есть только для него).
	if _, err := loadGlyph(ch.Correct().Emoji); err != nil {
		t.Fatalf("correct option has no glyph: %v", err)
	}
}

func TestUnknownModeFallsBackToCircles(t *testing.T) {
	c := New("")
	if len(c.Options) != len(circles) {
		t.Fatalf("empty mode: got %d, want %d (circles)", len(c.Options), len(circles))
	}
	c2 := New("nonsense")
	if len(c2.Options) != len(circles) {
		t.Fatalf("unknown mode: got %d, want %d (circles)", len(c2.Options), len(circles))
	}
}
