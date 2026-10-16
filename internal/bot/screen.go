package bot

import (
	"context"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// Виды ЛС-экранов (dm_screens.kind): сообщений-интерфейсов бота в личке,
// которые заменяют друг друга — повторный показ гасит прошлую копию, чтобы
// экраны не копились стопкой.
const (
	// screenPanel — экраны меню: /start, /chats, /help, /info, /whatsnew.
	// Открытие любого из них гасит прошлый экран: у бота в личке всегда
	// одна свежая панель, а не стопка старых.
	screenPanel = "panel"
	// screenLogs — сообщения команды /logs (файл лога и отбивки об ошибках):
	// новый вызов заменяет прошлое сообщение, а не копит файлы.
	screenLogs = "logs"
	// screenPrompt — флоу ввода текста «после капчи»: промпт, переспросы и
	// итоговые подтверждения живут как одно сообщение флоу.
	screenPrompt = "prompt"
	// screenNotice — мелкие служебные отбивки (отказы в доступе, пояснения):
	// повторная отбивка заменяет прошлую, но панели и флоу не трогает.
	screenNotice = "notice"
)

// sendScreen отправляет текстовый экран в личку и заменяет прошлый экран
// того же вида. html включает HTML-разметку; kb — клавиатура (nil = без).
// Упавшая отправка ничего не удаляет: прошлый экран обязан пережить сбой.
func (b *Bot) sendScreen(ctx context.Context, userID int64, kind, text string, html bool, kb *telego.InlineKeyboardMarkup) (*telego.Message, error) {
	params := tu.Message(tu.ID(userID), text)
	if html {
		params = params.WithParseMode(telego.ModeHTML)
	}
	if kb != nil {
		params = params.WithReplyMarkup(kb)
	}
	sent, err := b.api.SendMessage(ctx, params)
	if err != nil {
		return nil, err
	}
	b.replaceScreen(ctx, userID, kind, sent.MessageID)
	return sent, nil
}

// replaceScreen запоминает только что отправленный экран и удаляет прошлый
// экран того же вида. Вызывается ПОСЛЕ удачной отправки: упавшая отправка
// обязана оставить прошлый экран на месте. Удаление best-effort — сообщение
// могло быть стёрто юзером, устареть или упереться в сеть; сорванная чистка
// не повод ломать показ, поэтому в логе только Debug. Сам своп живёт на
// runCtx (запись в БД — это всегда runCtx, см. Context discipline), а
// удаление — на ctx вызова: если хендлер уже заканчивается, теряется лишь
// чистка, и прошлый экран останется до следующего показа.
func (b *Bot) replaceScreen(ctx context.Context, userID int64, kind string, messageID int) {
	old, err := b.db.SwapDMScreen(b.runCtx, userID, kind, messageID, time.Now())
	if err != nil {
		b.log.Warn("swap dm screen", "err", err, "user", userID, "kind", kind)
		return
	}
	if old == 0 {
		return
	}
	if err := b.deleteMessage(ctx, userID, old); err != nil {
		b.log.Debug("delete prev dm screen", "err", err, "user", userID, "kind", kind)
	}
}
