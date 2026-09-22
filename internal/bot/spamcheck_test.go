package bot

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestVoteVerdict(t *testing.T) {
	tests := []struct {
		yes, no, margin int
		wantSpam        bool
		wantDecided     bool
	}{
		{0, 0, 3, false, false},
		{2, 0, 3, false, false},
		{3, 0, 3, true, true},
		{5, 2, 3, true, true},
		{0, 3, 3, false, true},
		{2, 5, 3, false, true},
		{4, 2, 3, false, false}, // перевес 2 при пороге 3 — мало
		{12, 9, 3, true, true},  // пример юзера: 12-9 → бан
		{8, 11, 3, false, true}, // пример юзера: 8-11 → оправдание
		{2, 0, 2, true, true},   // порог 2 срабатывает раньше
		{4, 0, 5, false, false}, // порог 5 — 4:0 ещё мало
		{5, 0, 5, true, true},
	}
	for _, tc := range tests {
		spam, decided := voteVerdict(tc.yes, tc.no, tc.margin)
		if spam != tc.wantSpam || decided != tc.wantDecided {
			t.Errorf("voteVerdict(%d, %d, %d) = (%v, %v), want (%v, %v)",
				tc.yes, tc.no, tc.margin, spam, decided, tc.wantSpam, tc.wantDecided)
		}
	}
}

func TestUserLabel(t *testing.T) {
	tests := []struct {
		u    telego.User
		want string
	}{
		{telego.User{ID: 1, FirstName: "Вася", LastName: "Пупкин", Username: "vasya"}, "Вася Пупкин (@vasya, id1)"},
		{telego.User{ID: 2, FirstName: "Аня"}, "Аня (id2)"},
		{telego.User{ID: 3, Username: "ghost"}, "(без имени) (@ghost, id3)"},
	}
	for _, tc := range tests {
		if got := userLabel(tc.u); got != tc.want {
			t.Errorf("userLabel(%+v) = %q, want %q", tc.u, got, tc.want)
		}
	}
}

func TestEffectiveSpamSettings(t *testing.T) {
	var s storage.ChatSettings
	if effectiveSpamWhitelist(s) != defaultSpamWhitelist {
		t.Errorf("NULL whitelist must fall back to %d", defaultSpamWhitelist)
	}
	s.SpamWhitelistMsgs = sql.NullInt64{Int64: 10, Valid: true}
	if effectiveSpamWhitelist(s) != 10 {
		t.Error("valid override must win")
	}
	// Мусор в БД → дефолт.
	s.SpamWhitelistMsgs = sql.NullInt64{Int64: -3, Valid: true}
	if effectiveSpamWhitelist(s) != defaultSpamWhitelist {
		t.Error("non-positive whitelist must fall back")
	}

	var m storage.ChatSettings
	if effectiveSpamVoteMargin(m) != defaultSpamVoteMargin {
		t.Errorf("NULL margin must fall back to %d", defaultSpamVoteMargin)
	}
	m.SpamVoteMargin = sql.NullInt64{Int64: 5, Valid: true}
	if effectiveSpamVoteMargin(m) != 5 {
		t.Error("valid margin override must win")
	}
	m.SpamVoteMargin = sql.NullInt64{Int64: 99, Valid: true}
	if effectiveSpamVoteMargin(m) != defaultSpamVoteMargin {
		t.Error("out-of-range margin must fall back")
	}
}

func TestBuildSpamFacts(t *testing.T) {
	m := telego.Message{
		From: &telego.User{FirstName: "Вася", LastName: "Пупкин", Username: "vasya"},
		Text: "Заработок от 500$ в день, пиши в личку!",
	}
	facts := buildSpamFacts(m, "5 минут", 1)
	for _, want := range []string{"Вася Пупкин", "@vasya", "в чате 5 минут", "всего сообщений: 1", "Заработок от 500$"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts missing %q:\n%s", want, facts)
		}
	}

	// Гифка без текста + форвард из канала.
	m2 := telego.Message{
		From:      &telego.User{FirstName: "X"},
		Animation: &telego.Animation{},
		Document:  &telego.Document{}, // гифки выставляют оба поля
		ForwardOrigin: &telego.MessageOriginChannel{
			Chat: telego.Chat{Title: "Крипта Сигналы"},
		},
	}
	facts2 := buildSpamFacts(m2, "", 1)
	for _, want := range []string{"гифка, без текста", "Переслано: канал «Крипта Сигналы»"} {
		if !strings.Contains(facts2, want) {
			t.Errorf("facts missing %q:\n%s", want, facts2)
		}
	}
	if strings.Contains(facts2, "файл") {
		t.Errorf("animation must not be reported as document:\n%s", facts2)
	}

	// Фото с подписью.
	m3 := telego.Message{
		From:    &telego.User{FirstName: "Y"},
		Photo:   []telego.PhotoSize{{}},
		Caption: "Скидки на всё!",
	}
	facts3 := buildSpamFacts(m3, "", 2)
	for _, want := range []string{"фото с подписью", "Скидки на всё!"} {
		if !strings.Contains(facts3, want) {
			t.Errorf("facts missing %q:\n%s", want, facts3)
		}
	}

	// Реплай с цитатой от автора.
	m4 := telego.Message{
		From: &telego.User{FirstName: "Ответчик", Username: "replier"},
		Text: "Да, видел это видео, смешно!",
		ReplyToMessage: &telego.Message{
			From: &telego.User{FirstName: "Оригинал", Username: "original"},
			Text: "Слили ВИДЕО С БРАТОМ И СЕСТРОЙ И ПАНТЕРОЙ",
		},
	}
	facts4 := buildSpamFacts(m4, "", 5)
	for _, want := range []string{"Цитата от Оригинал (@original)", "Слили ВИДЕО С БРАТОМ И СЕСТРОЙ И ПАНТЕРОЙ", "Да, видел это видео"} {
		if !strings.Contains(facts4, want) {
			t.Errorf("facts missing %q:\n%s", want, facts4)
		}
	}

	// Реплай — форвард из канала.
	m5 := telego.Message{
		From: &telego.User{FirstName: "Z"},
		Text: "Ок",
		ReplyToMessage: &telego.Message{
			ForwardOrigin: &telego.MessageOriginChannel{
				Chat: telego.Chat{Title: "СпамКанал"},
			},
			Text: "Купи крипту!",
		},
	}
	facts5 := buildSpamFacts(m5, "", 1)
	for _, want := range []string{"Цитата", ", переслано: канал «СпамКанал»", "Купи крипту!"} {
		if !strings.Contains(facts5, want) {
			t.Errorf("facts missing %q:\n%s", want, facts5)
		}
	}

	// Реплай без текста — только фото.
	m6 := telego.Message{
		From: &telego.User{FirstName: "W"},
		Text: "Вот",
		ReplyToMessage: &telego.Message{
			From:  &telego.User{FirstName: "Фотограф"},
			Photo: []telego.PhotoSize{{}},
		},
	}
	facts6 := buildSpamFacts(m6, "", 1)
	if !strings.Contains(facts6, "Цитата от Фотограф") {
		t.Errorf("facts missing quote author:\n%s", facts6)
	}

	// Реплай без автора и форварда (service message).
	m7 := telego.Message{
		From: &telego.User{FirstName: "X"},
		Text: "комментарий",
		ReplyToMessage: &telego.Message{
			Text: "autogenerated",
		},
	}
	facts7 := buildSpamFacts(m7, "", 1)
	for _, want := range []string{"Цитата", "autogenerated", "комментарий"} {
		if !strings.Contains(facts7, want) {
			t.Errorf("facts missing %q:\n%s", want, facts7)
		}
	}

	// external_reply из канала + цитата + скрытый форвард.
	m8 := telego.Message{
		From: &telego.User{FirstName: "А"},
		Text: "Слили видео, заходи!",
		ExternalReply: &telego.ExternalReplyInfo{
			Origin: &telego.MessageOriginChannel{
				Chat: telego.Chat{Title: "СЛИВЫ", Username: "sliv"},
			},
			Photo: []telego.PhotoSize{{}},
		},
		Quote: &telego.TextQuote{Text: "🔥 эксклюзив"},
		ForwardOrigin: &telego.MessageOriginHiddenUser{
			SenderUserName: "Deleted Account",
		},
	}
	facts8 := buildSpamFacts(m8, "", 1)
	for _, want := range []string{
		"Переслано: Deleted Account",
		"Реплай на сообщение из: канал «СЛИВЫ» (@sliv)",
		"Цитата:\n🔥 эксклюзив",
		"Вложение оригинала: фото",
		"Слили видео, заходи!",
	} {
		if !strings.Contains(facts8, want) {
			t.Errorf("facts missing %q:\n%s", want, facts8)
		}
	}
	if strings.Contains(facts8, "-100") || strings.Contains(facts8, "(id") {
		t.Errorf("facts must not leak chat ids:\n%s", facts8)
	}

	// Форвард от обычного юзера: имя + @username.
	m9 := telego.Message{
		From:          &telego.User{FirstName: "Б"},
		Text:          "ок",
		ForwardOrigin: &telego.MessageOriginUser{SenderUser: telego.User{ID: 7, FirstName: "Иван", Username: "ivan"}},
	}
	facts9 := buildSpamFacts(m9, "", 1)
	if !strings.Contains(facts9, "Переслано: Иван (@ivan)") {
		t.Errorf("facts missing user forward label:\n%s", facts9)
	}

	// Лог /spam видит те же поля.
	targetCtx := spamTargetContext(m8)
	for _, want := range []string{
		"fwd:Deleted Account",
		"ext_src:канал «СЛИВЫ» (@sliv)",
		"ext_media:фото",
		`ext_quote:"🔥 эксклюзив"`,
	} {
		if !strings.Contains(targetCtx, want) {
			t.Errorf("target_ctx missing %q:\n%s", want, targetCtx)
		}
	}
}

func TestHumanDurationRU(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "меньше минуты"},
		{5 * time.Minute, "5 минут"},
		{2 * time.Hour, "2 часа"},
		{72 * time.Hour, "3 дня"},
	}
	for _, tc := range tests {
		if got := humanDurationRU(tc.d); got != tc.want {
			t.Errorf("humanDurationRU(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
