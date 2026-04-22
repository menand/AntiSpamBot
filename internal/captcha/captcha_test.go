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
	for i := 0; i < 100; i++ {
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
	for i := 0; i < 100; i++ {
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
	// Build reverse index: emoji → category id.
	emojiToCat := make(map[string]int, 64)
	for catIdx, cat := range emojiCategories {
		for _, tok := range cat {
			emojiToCat[tok.Emoji] = catIdx
		}
	}

	// Run many iterations; every challenge must cover every category exactly once.
	const iterations = 200
	categoryHit := make([]int, len(emojiCategories)) // total counts across all runs
	for i := 0; i < iterations; i++ {
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

	// Sanity: every category should have been hit many times across 200 iterations.
	for i, n := range categoryHit {
		if n != iterations {
			t.Errorf("category %d hit %d times, expected %d (one per iteration)", i, n, iterations)
		}
	}
}

func TestEmojiShufflesCategoryOrder(t *testing.T) {
	// After shuffling, the emoji at slot 0 should sometimes come from different
	// categories across runs. If slot 0 were always category 0, the shuffle
	// would be broken.
	emojiToCat := make(map[string]int, 64)
	for catIdx, cat := range emojiCategories {
		for _, tok := range cat {
			emojiToCat[tok.Emoji] = catIdx
		}
	}
	distinctFirstCats := make(map[int]struct{})
	for i := 0; i < 200; i++ {
		c := New(ModeEmoji)
		distinctFirstCats[emojiToCat[c.Options[0].Emoji]] = struct{}{}
		if len(distinctFirstCats) >= 3 {
			return
		}
	}
	t.Fatalf("across 200 runs, slot 0 only came from %d distinct categories; shuffle is broken",
		len(distinctFirstCats))
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
