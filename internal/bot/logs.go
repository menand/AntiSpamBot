package bot

import (
	"fmt"
	"os"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

// Telegram bot API file upload limit is 50 MB.
const maxLogUploadBytes = 50 * 1024 * 1024

func (b *Bot) handleLogsCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "private" {
		return nil
	}
	if message.From == nil || !b.isOwner(message.From.ID) {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			"Команда доступна только владельцам бота (OWNER_IDS)."))
		return nil
	}
	if b.cfg.LogFile == "" {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			"LOG_FILE не задан в настройках — бот не пишет логи в файл. "+
				"Для Docker compose он обычно выставляется автоматически в <code>/data/bot.log</code>. "+
				"Если ты деплоишь вручную — укажи <code>LOG_FILE=/path/to/bot.log</code> в окружении.").
			WithParseMode(telego.ModeHTML))
		return nil
	}

	file, err := os.Open(b.cfg.LogFile)
	if err != nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			fmt.Sprintf("Не удалось открыть лог-файл (%s): %v", b.cfg.LogFile, err)))
		return nil
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			fmt.Sprintf("stat лог-файла упал: %v", err)))
		return nil
	}
	if info.Size() == 0 {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			"Лог-файл пустой."))
		return nil
	}
	if info.Size() > maxLogUploadBytes {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			fmt.Sprintf(
				"Файл лога больше 50 МБ (%.1f МБ) — Telegram не даёт отправить ботом такой размер. "+
					"Файл автоматически ротируется при 10 МБ; попробуй позже или забери с VDS вручную.",
				float64(info.Size())/1024/1024)))
		return nil
	}

	filename := fmt.Sprintf("bot-%s.log", time.Now().UTC().Format("2006-01-02-150405"))
	_, err = b.api.SendDocument(ctx,
		tu.Document(tu.ID(message.Chat.ID), tu.File(tu.NameReader(file, filename))).
			WithCaption(fmt.Sprintf("📄 %s (%.1f КБ)", filename, float64(info.Size())/1024)))
	if err != nil {
		b.log.Warn("send logs", "err", err)
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			fmt.Sprintf("Ошибка отправки: %v", err)))
	}
	return nil
}
