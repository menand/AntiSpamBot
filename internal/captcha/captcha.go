package captcha

import "math/rand/v2"

type Color struct {
	Emoji string
	Name  string
}

var palette = []Color{
	{"🔴", "красный"},
	{"🟢", "зелёный"},
	{"🔵", "синий"},
	{"🟡", "жёлтый"},
	{"🟣", "фиолетовый"},
	{"🟠", "оранжевый"},
}

type Challenge struct {
	Options    []Color
	CorrectIdx int
}

func (c Challenge) Correct() Color {
	return c.Options[c.CorrectIdx]
}

func New() Challenge {
	opts := make([]Color, len(palette))
	copy(opts, palette)
	rand.Shuffle(len(opts), func(i, j int) {
		opts[i], opts[j] = opts[j], opts[i]
	})
	return Challenge{
		Options:    opts,
		CorrectIdx: rand.IntN(len(opts)),
	}
}
