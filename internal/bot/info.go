package bot

import (
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (b *Bot) handleInfoCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "private" {
		return nil
	}
	if message.From == nil || !b.isOwner(message.From.ID) {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			"Команда доступна только владельцам бота (OWNER_IDS)."))
		return nil
	}

	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		loc = time.UTC
	}
	started := b.startedAt.In(loc).Format("02.01.2006 15:04:05 MST")
	uptime := time.Since(b.startedAt)

	text := fmt.Sprintf(
		"🤖 <b>Информация о боте</b>\n\n"+
			"Username: @%s\n"+
			"Запущен: <code>%s</code>\n"+
			"Работает: <b>%s</b>",
		b.Username(),
		started,
		formatUptimeRU(uptime),
	)
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML))
	return nil
}

// formatUptimeRU renders a duration like "3 дня, 5 часов, 42 минуты".
// Trailing zero components are omitted; seconds are shown only for very short
// uptimes (less than a minute).
func formatUptimeRU(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	secs := int((d % time.Minute) / time.Second)

	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", days, pluralRU(days, "день", "дня", "дней")))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", hours, pluralRU(hours, "час", "часа", "часов")))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", mins, pluralRU(mins, "минута", "минуты", "минут")))
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d %s", secs, pluralRU(secs, "секунда", "секунды", "секунд")))
	}
	return strings.Join(parts, ", ")
}
