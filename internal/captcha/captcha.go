package captcha

import "math/rand/v2"

// Token — одна emoji-кнопка на клавиатуре капчи в паре с существительным
// в винительном падеже, которое подставляется в задание («Выбери <b>%s</b> …»).
type Token struct {
	Emoji  string
	Prompt string
}

// Mode выбирает, из какого пула капча берёт варианты.
type Mode string

const (
	ModeCircles Mode = "circles" // по умолчанию: 6 цветных кружков
	ModeEmoji   Mode = "emoji"   // по одному emoji из каждой из 6 категорий
	ModeImage   Mode = "image"   // варианты-emoji + искажённое фото глифа в роли задания
)

// circles: легаси-палитра «светофор». Все 6 показываются каждый раз.
var circles = []Token{
	{"🔴", "красный кружок"},
	{"🟢", "зелёный кружок"},
	{"🔵", "синий кружок"},
	{"🟡", "жёлтый кружок"},
	{"🟣", "фиолетовый кружок"},
	{"🟠", "оранжевый кружок"},
}

// emojiCategories: 6 визуально различимых групп. Для каждой капчи берём по
// одному случайному элементу из каждой → на клавиатуре всегда один «мир» на слот.
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

// New собирает новое задание капчи для заданного режима. Неизвестный/пустой
// режим откатывается к ModeCircles. ModeImage переиспользует пул emoji —
// разница только в подаче (бот рендерит правильный глиф искажённым фото
// вместо того, чтобы назвать его текстом).
func New(mode Mode) Challenge {
	switch mode {
	case ModeEmoji, ModeImage:
		return newEmoji()
	case ModeCircles:
		return newCircles()
	default:
		return newCircles()
	}
}

func newCircles() Challenge {
	opts := make([]Token, len(circles))
	copy(opts, circles)
	rand.Shuffle(len(opts), func(i, j int) { //nolint:gosec // captcha shuffle, not security-critical
		opts[i], opts[j] = opts[j], opts[i]
	})
	return Challenge{
		Options:    opts,
		CorrectIdx: rand.IntN(len(opts)), //nolint:gosec // captcha, not security-critical
	}
}

func newEmoji() Challenge {
	opts := make([]Token, 0, len(emojiCategories))
	for _, cat := range emojiCategories {
		opts = append(opts, cat[rand.IntN(len(cat))]) //nolint:gosec // captcha, not security-critical
	}
	rand.Shuffle(len(opts), func(i, j int) { //nolint:gosec // captcha, not security-critical
		opts[i], opts[j] = opts[j], opts[i]
	})
	return Challenge{
		Options:    opts,
		CorrectIdx: rand.IntN(len(opts)), //nolint:gosec // captcha, not security-critical
	}
}
