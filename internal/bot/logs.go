package bot

import (
	"fmt"
	"os"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

// Лимит Telegram Bot API на загрузку файла ботом — 50 МБ.
const maxLogUploadBytes = 50 * 1024 * 1024

func (b *Bot) handleLogsCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "private" {
		return nil
	}
	if message.From == nil || !b.isOwner(message.From.ID) {
		// Отбивка-отказ — экран screenNotice: повторный отказ заменит
		// прошлый, но экраны панели и флоу не тронет.
		_, _ = b.sendScreen(ctx, message.Chat.ID, screenNotice,
			"Команда доступна только владельцам бота (OWNER_IDS).", false, nil)
		return nil
	}
	// Все сообщения /logs — один заменяемый экран screenLogs: повторный вызов
	// гасит прошлый файл/отбивку, файлы не копятся стопкой.
	notice := func(text string, html bool) {
		if _, err := b.sendScreen(ctx, message.Chat.ID, screenLogs, text, html, nil); err != nil {
			b.log.Warn("send logs notice", "err", err, "user", message.From.ID)
		}
	}
	if b.cfg.LogFile == "" {
		notice("LOG_FILE не задан в настройках — бот не пишет логи в файл. "+
			"Для Docker compose он обычно выставляется автоматически в <code>/data/bot.log</code>. "+
			"Если ты деплоишь вручную — укажи <code>LOG_FILE=/path/to/bot.log</code> в окружении.", true)
		return nil
	}

	file, err := os.Open(b.cfg.LogFile)
	if err != nil {
		notice(fmt.Sprintf("Не удалось открыть лог-файл (%s): %v", b.cfg.LogFile, err), false)
		return nil
	}
	defer file.Close() //nolint:errcheck // cleanup, error intentionally ignored

	info, err := file.Stat()
	if err != nil {
		notice(fmt.Sprintf("stat лог-файла упал: %v", err), false)
		return nil
	}
	if info.Size() == 0 {
		notice("Лог-файл пустой.", false)
		return nil
	}
	if info.Size() > maxLogUploadBytes {
		notice(fmt.Sprintf(
			"Файл лога больше 50 МБ (%.1f МБ) — Telegram не даёт отправить ботом такой размер. "+
				"Файл автоматически ротируется при 10 МБ; попробуй позже или забери с VDS вручную.",
			float64(info.Size())/1024/1024), false)
		return nil
	}

	filename := fmt.Sprintf("bot-%s.log", time.Now().UTC().Format("2006-01-02-150405"))
	sent, err := b.api.SendDocument(ctx,
		tu.Document(tu.ID(message.Chat.ID), tu.File(tu.NameReader(file, filename))).
			WithCaption(fmt.Sprintf("📄 %s (%.1f КБ)", filename, float64(info.Size())/1024)))
	if err != nil {
		b.log.Warn("send logs", "err", err)
		notice(fmt.Sprintf("Ошибка отправки: %v", err), false)
		return nil
	}
	b.replaceScreen(ctx, message.Chat.ID, screenLogs, sent.MessageID)
	return nil
}
