package captcha

import "math/rand/v2"

// Token is one emoji button on the captcha keyboard, paired with its
// accusative-case noun phrase used in the prompt ("Выбери <b>%s</b> …").
type Token struct {
	Emoji  string
	Prompt string
}

// Mode selects which pool the captcha draws from.
type Mode string

const (
	ModeCircles Mode = "circles" // default: 6 colored circles
	ModeEmoji   Mode = "emoji"   // one emoji from each of 6 categories
)

// circles: the legacy "traffic-light" palette. All 6 are shown every time.
var circles = []Token{
	{"🔴", "красный кружок"},
	{"🟢", "зелёный кружок"},
	{"🔵", "синий кружок"},
	{"🟡", "жёлтый кружок"},
	{"🟣", "фиолетовый кружок"},
	{"🟠", "оранжевый кружок"},
}

// emojiCategories: 6 visually-distinct groups. For every captcha we pick one
// random item from each → the keyboard always has one "world" per slot.
var emojiCategories = [][]Token{
	{ // Звери
		{"🦊", "лису"},
		{"🦁", "льва"},
		{"🐼", "панду"},
		{"🐒", "обезьяну"},
		{"🐘", "слона"},
		{"🦒", "жирафа"},
		{"🦝", "енота"},
		{"🦄", "единорога"},
	},
	{ // Летуны
		{"🦋", "бабочку"},
		{"🐝", "пчелу"},
		{"🦉", "сову"},
		{"🦅", "орла"},
		{"🐧", "пингвина"},
		{"🐣", "цыплёнка"},
	},
	{ // Плавучие и ползучие
		{"🐢", "черепаху"},
		{"🐙", "осьминога"},
		{"🐟", "рыбу"},
		{"🐬", "дельфина"},
		{"🐊", "крокодила"},
		{"🐍", "змею"},
		{"🐸", "лягушку"},
	},
	{ // Природа и погода
		{"☀️", "солнышко"},
		{"🌙", "луну"},
		{"⭐", "звёздочку"},
		{"🌈", "радугу"},
		{"❄️", "снежинку"},
		{"🔥", "огонь"},
		{"🌸", "цветочек"},
		{"🌻", "подсолнух"},
		{"🍄", "гриб"},
	},
	{ // Еда
		{"🍎", "яблоко"},
		{"🍌", "банан"},
		{"🍓", "клубнику"},
		{"🍕", "пиццу"},
		{"🍩", "пончик"},
		{"🍋", "лимон"},
		{"🍉", "арбуз"},
	},
	{ // Вещи
		{"🎈", "шарик"},
		{"🎁", "подарок"},
		{"🚀", "ракету"},
		{"⚽", "мяч"},
		{"🎸", "гитару"},
		{"📚", "книгу"},
	},
}

type Challenge struct {
	Options    []Token
	CorrectIdx int
}

func (c Challenge) Correct() Token {
	return c.Options[c.CorrectIdx]
}

// New builds a fresh captcha challenge for the given mode. Unknown/empty
// modes fall back to ModeCircles.
func New(mode Mode) Challenge {
	if mode == ModeEmoji {
		return newEmoji()
	}
	return newCircles()
}

func newCircles() Challenge {
	opts := make([]Token, len(circles))
	copy(opts, circles)
	rand.Shuffle(len(opts), func(i, j int) {
		opts[i], opts[j] = opts[j], opts[i]
	})
	return Challenge{
		Options:    opts,
		CorrectIdx: rand.IntN(len(opts)),
	}
}

func newEmoji() Challenge {
	opts := make([]Token, 0, len(emojiCategories))
	for _, cat := range emojiCategories {
		opts = append(opts, cat[rand.IntN(len(cat))])
	}
	rand.Shuffle(len(opts), func(i, j int) {
		opts[i], opts[j] = opts[j], opts[i]
	})
	return Challenge{
		Options:    opts,
		CorrectIdx: rand.IntN(len(opts)),
	}
}
