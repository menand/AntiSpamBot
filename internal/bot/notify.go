package bot

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// modNotifyTargets — владельцы, включившие ЛС-уведомления о киках/банах.
// Пересечение с OWNER_IDS отсеивает строки бывших владельцев (как у спама).
func (b *Bot) modNotifyTargets(ctx context.Context) []int64 {
	ids, err := b.db.ModNotifyOwners(ctx)
	if err != nil {
		b.log.Warn("mod notify owners", "err", err)
		return nil
	}
	var out []int64
	for _, id := range ids {
		if b.isOwner(id) {
			out = append(out, id)
		}
	}
	return out
}

// notifyModAction шлёт подписанным владельцам карточку кика/бана: чат, цель,
// действие, человекочитаемая причина. Vote-вердикты сюда НЕ идут — они уже
// покрыты spam_notify (notifySpamVerdict); reason им передаётся только для
// events, а дубль-уведомление было бы шумом.
func (b *Bot) notifyModAction(chatID, targetID int64, kind storage.EventKind, reason string) {
	targets := b.modNotifyTargets(b.runCtx)
	if len(targets) == 0 {
		return
	}
	action := "👢 Кик"
	if kind == storage.EventBan || kind == storage.EventSpamBan {
		action = "🚫 Бан"
	}
	infos, _ := b.db.GetUserInfos(b.runCtx, []int64{targetID})
	text := fmt.Sprintf("%s в «%s»\nКого: %s\nПричина: %s",
		action, html.EscapeString(b.chatTitle(b.runCtx, chatID)),
		mentionWithUsername(infos, targetID), b.humanReason(reason))
	for _, ownerID := range targets {
		if _, err := b.api.SendMessage(b.runCtx, tu.Message(tu.ID(ownerID), text).
			WithParseMode(telego.ModeHTML)); err != nil {
			b.log.Warn("notify mod action", "err", err, "owner", ownerID)
		}
	}
}

// humanReason разворачивает reason события (см. storage.Reason*) в
// человекочитаемую строку для ЛС-уведомления: имена админов/голосовавших
// резолвятся точечным запросом в БД. Пустой reason (старые строки) → "".
func (b *Bot) humanReason(reason string) string {
	return humanReasonWith(reason, func(ids []int64) map[int64]storage.UserInfo {
		infos, _ := b.db.GetUserInfos(b.runCtx, ids)
		return infos
	})
}

// humanReasonWith — чистое ядро разбора причины: nameLookup отдаёт кэш имён
// для нужных id (в БД или из готовой map). Готово к вставке в ModeHTML.
func humanReasonWith(reason string, nameLookup func(ids []int64) map[int64]storage.UserInfo) string {
	switch {
	case reason == "":
		return ""
	case reason == storage.ReasonCaptcha:
		return "не прошёл капчу"
	case reason == storage.ReasonNoReply:
		return "не ответил на приветствие"
	case reason == storage.ReasonGlobal:
		return "в глобальной базе спамеров"
	case strings.HasPrefix(reason, storage.ReasonModPrefix):
		adminID, _ := parseModID(reason)
		return "команда админа " + mentionWithUsername(nameLookup([]int64{adminID}), adminID)
	case strings.HasPrefix(reason, storage.ReasonVotePrefix):
		ids := parseVoteIDs(reason)
		if len(ids) == 0 {
			return "голосование чата"
		}
		infos := nameLookup(ids)
		names := make([]string, len(ids))
		for i, id := range ids {
			names[i] = mentionWithUsername(infos, id)
		}
		return "голосование: " + strings.Join(names, ", ")
	}
	return html.EscapeString(reason)
}

// reasonUserIDs собирает id, которые причины событий (mod:/vote:) захотят
// показать по имени — чтобы renderStats добрал их в общий infos-запрос.
func reasonUserIDs(lists ...[]storage.UserCount) []int64 {
	var out []int64
	for _, l := range lists {
		for _, uc := range l {
			switch {
			case strings.HasPrefix(uc.LastReason, storage.ReasonModPrefix):
				if id, ok := parseModID(uc.LastReason); ok {
					out = append(out, id)
				}
			case strings.HasPrefix(uc.LastReason, storage.ReasonVotePrefix):
				out = append(out, parseVoteIDs(uc.LastReason)...)
			}
		}
	}
	return out
}

// parseModID достаёт adminID из reason «mod:<id>».
func parseModID(reason string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(reason, storage.ReasonModPrefix), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// parseVoteIDs достаёт id голосовавших «за» из reason «vote:1,2,3».
func parseVoteIDs(reason string) []int64 {
	raw := strings.TrimPrefix(reason, storage.ReasonVotePrefix)
	if raw == "" {
		return nil
	}
	var out []int64
	for _, s := range strings.Split(raw, ",") {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}
