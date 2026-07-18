package bot

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	antispam "github.com/menand/AntiSpamBot"
)

const (
	// whatsNewVersions — сколько последних версий показывает /whatsnew.
	whatsNewVersions = 10
	// whatsNewRuneBudget — потолок длины сообщения (лимит Telegram 4096,
	// запас на заголовок и «…»).
	whatsNewRuneBudget = 3500

	// metaAnnouncedVersion — ключ bot_meta: последняя версия, о которой
	// разосланы ЛС-оповещения.
	metaAnnouncedVersion = "announced_version"
)

// release — одна запись CHANGELOG.md.
type release struct {
	Version string // «v1.6.0»
	Date    string // «2026-07-18»
	Items   []string
}

// parseChangelog разбирает встроенный CHANGELOG.md: «## vX.Y.Z — дата» +
// пункты «- …»; прочие строки игнорируются. Порядок файла сохраняется (новые
// версии сверху). Формат сторожит TestEmbeddedChangelogParses.
func parseChangelog(md string) []release {
	var out []release
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "## "):
			ver, date, _ := strings.Cut(strings.TrimPrefix(line, "## "), "—")
			out = append(out, release{
				Version: strings.TrimSpace(ver),
				Date:    strings.TrimSpace(date),
			})
		case strings.HasPrefix(line, "- ") && len(out) > 0:
			out[len(out)-1].Items = append(out[len(out)-1].Items,
				strings.TrimSpace(strings.TrimPrefix(line, "- ")))
		}
	}
	return out
}

// describeSuffixRe — хвост git describe: «-<коммитов после тега>-g<хеш>».
var describeSuffixRe = regexp.MustCompile(`-\d+-g[0-9a-f]+$`)

// baseVersion выделяет из версии сборки релизный тег: «v1.6.0-3-gabc123
// [-dirty]» → «v1.6.0». Для dev-сборок (go run без ldflags) — пустая строка:
// сравнивать и анонсировать нечего.
func baseVersion(v string) string {
	if v == "" || v == "dev" {
		return ""
	}
	v = strings.TrimSuffix(v, "-dirty")
	return describeSuffixRe.ReplaceAllString(v, "")
}

// renderWhatsNew — HTML последних limit версий в рунный бюджет; не влезшие
// версии сворачиваются в «…».
func renderWhatsNew(rels []release, limit int) string {
	var sb strings.Builder
	sb.WriteString("🆕 <b>Что нового</b>")
	used, shown := 0, 0
	for _, r := range rels {
		if shown == limit {
			break
		}
		var block strings.Builder
		fmt.Fprintf(&block, "\n\n<b>%s</b>", html.EscapeString(r.Version))
		if r.Date != "" {
			block.WriteString(" — " + html.EscapeString(r.Date))
		}
		for _, it := range r.Items {
			block.WriteString("\n• " + html.EscapeString(it))
		}
		if used += utf8.RuneCountInString(block.String()); used > whatsNewRuneBudget {
			sb.WriteString("\n\n…")
			break
		}
		sb.WriteString(block.String())
		shown++
	}
	return sb.String()
}

// handleWhatsNewCommand — /whatsnew и /whatnew: краткий чейнджлог последних
// версий. В ЛС — обычным сообщением; в группе — по образцу /help: ВСЕГДА
// эфемерно (ответ адресован одному юзеру), команда удаляется, анонимному
// отправителю (From = GroupAnonymousBot) — публично.
func (b *Bot) handleWhatsNewCommand(ctx *th.Context, message telego.Message) error {
	text := renderWhatsNew(parseChangelog(antispam.ChangelogMD), whatsNewVersions)
	if message.Chat.Type == "private" {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
			WithParseMode(telego.ModeHTML))
		return nil
	}
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	chatID := message.Chat.ID
	if !b.chatAllowed(chatID) || message.From == nil || !b.commandForUs(message.Text) {
		return nil
	}
	var recv int64
	if !message.From.IsBot {
		recv = message.From.ID
	}
	b.sendHTML(chatID, threadOf(message), recv, text)
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete /whatsnew command", "err", err, "chat", chatID)
	}
	return nil
}

// announceVersion — single-shot на старте: если релизная (базовая) версия
// сборки изменилась с прошлого запуска, разослать владельцам и админам всех
// зарегистрированных чатов ЛС «бот обновлён» с тезисами версии из чейнджлога.
// Маркер пишется ДО рассылки: краш-луп не должен спамить, недоставленное
// догонит следующий релиз. Отправка best-effort: закрытая ЛС (юзер не
// открывал диалог с ботом) — норма, не ошибка.
func (b *Bot) announceVersion(ctx context.Context) {
	base := baseVersion(b.version)
	if base == "" {
		return
	}
	last, err := b.db.GetMeta(ctx, metaAnnouncedVersion)
	if err != nil {
		b.log.Warn("announce version: get meta", "err", err)
		return
	}
	if last == base {
		return
	}
	if err := b.db.SetMeta(ctx, metaAnnouncedVersion, base); err != nil {
		b.log.Warn("announce version: set meta", "err", err)
		return
	}

	targets := make(map[int64]struct{}, len(b.cfg.OwnerIDs))
	for id := range b.cfg.OwnerIDs {
		targets[id] = struct{}{}
	}
	chats, err := b.db.ListChats(ctx)
	if err != nil {
		b.log.Warn("announce version: list chats", "err", err)
	}
	for _, c := range chats {
		if (c.Type != "group" && c.Type != "supergroup") || !b.chatAllowed(c.ChatID) {
			continue
		}
		admins, err := b.api.GetChatAdministrators(ctx,
			&telego.GetChatAdministratorsParams{ChatID: tu.ID(c.ChatID)})
		if err != nil {
			b.log.Warn("announce version: chat admins", "err", err, "chat", c.ChatID)
			continue
		}
		for _, m := range admins {
			if u := m.MemberUser(); !u.IsBot {
				targets[u.ID] = struct{}{}
			}
		}
	}
	// Индивидуальный opt-out (кнопка «🆕» в меню): вычитаем отказников одним
	// запросом. Ошибка чтения — шлём всем: потерянный анонс дороже
	// недоставленной отписки, отказников единицы.
	if optOuts, oerr := b.db.VersionNotifyOptOuts(ctx); oerr != nil {
		b.log.Warn("announce version: opt-outs", "err", oerr)
	} else {
		for _, id := range optOuts {
			delete(targets, id)
		}
	}

	text := "🆕 <b>Бот обновлён: " + html.EscapeString(base) + "</b>"
	for _, r := range parseChangelog(antispam.ChangelogMD) {
		if r.Version == base {
			for _, it := range r.Items {
				text += "\n• " + html.EscapeString(it)
			}
			break
		}
	}
	text += "\n\nℹ️ Полный список команд — по команде /help." +
		"\n🔕 Отключить эти оповещения: /start → «🆕 Оповещения о версиях»."
	sent := 0
	for id := range targets {
		if _, err := b.api.SendMessage(ctx, tu.Message(tu.ID(id), text).
			WithParseMode(telego.ModeHTML)); err != nil {
			// Telegram не даёт боту писать первым — юзер не открывал ЛС.
			b.log.Debug("announce version: dm", "err", err, "user", id)
			continue
		}
		sent++
	}
	b.log.Info("version announced", "version", base,
		"recipients", len(targets), "sent", sent)
}
