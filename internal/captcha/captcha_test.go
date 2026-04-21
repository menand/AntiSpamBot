package captcha

import "testing"

func TestNewProducesAllDistinctColors(t *testing.T) {
	c := New()
	if len(c.Options) != len(palette) {
		t.Fatalf("got %d options, want %d", len(c.Options), len(palette))
	}
	seen := make(map[string]struct{}, len(c.Options))
	for _, opt := range c.Options {
		if _, dup := seen[opt.Emoji]; dup {
			t.Fatalf("duplicate emoji %q in options", opt.Emoji)
		}
		seen[opt.Emoji] = struct{}{}
	}
}

func TestCorrectIdxInRange(t *testing.T) {
	for i := 0; i < 100; i++ {
		c := New()
		if c.CorrectIdx < 0 || c.CorrectIdx >= len(c.Options) {
			t.Fatalf("CorrectIdx %d out of range (len=%d)", c.CorrectIdx, len(c.Options))
		}
		if c.Correct() != c.Options[c.CorrectIdx] {
			t.Fatal("Correct() disagrees with Options[CorrectIdx]")
		}
	}
}

func TestShufflePermutesOrder(t *testing.T) {
	// Very low probability that 100 shuffles of 6 items all match original order.
	var differs bool
	for i := 0; i < 100; i++ {
		c := New()
		for j, opt := range c.Options {
			if opt.Emoji != palette[j].Emoji {
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
