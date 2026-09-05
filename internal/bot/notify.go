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

// modNotifyTargets — владельцы, включившие ЛС-уведомления о киках/банах и
// проходах капчи.
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

// captchaNotifyTargets — владельцы, подписанные на ВСЕ провалы капчи
// (тумблер «🧩», captcha_notify). Фильтр isOwner — как у modNotifyTargets.
func (b *Bot) captchaNotifyTargets(ctx context.Context) []int64 {
	ids, err := b.db.CaptchaNotifyOwners(ctx)
	if err != nil {
		b.log.Warn("captcha notify owners", "err", err)
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

// notifyModAction шлёт подписчикам mod_notify карточку кика/бана (а для
// EventPass — прохода капчи): чат, цель, действие, человекочитаемая причина.
// Провалы капчи сюда НЕ ходят — у них свой notifyCaptchaFail (отдельная
// подписка + порог попыток). Vote-вердикты тоже НЕ идут — они уже покрыты
// spam_notify (notifySpamVerdict); reason им передаётся только для events, а
// дубль-уведомление было бы шумом.
// detail — необязательное уточнение к причине (для pass — «выбрал 2-й (🟢)»);
// в events не пишется, живёт только здесь.
// Уведомление уходит в горутине (как spamVerdictFanout): вызывающие стоят на
// карательном пути (onFail/replyWaitLoop и captchaStageLoop с 10-секундным cleanup-ctx), и
// зависший SendMessage не должен съедать бюджет kick/ban.
func (b *Bot) notifyModAction(chatID, targetID int64, kind storage.EventKind, reason string, detail ...string) {
	b.goSafe("notifyModAction", func() {
		b.sendModCard(b.modNotifyTargets(b.runCtx), chatID, targetID, kind, reason, detail...)
	})
}

// notifyCaptchaFail — провал капчи: подписчикам captcha_notify (все провалы)
// и, со второй попытки, подписчикам общего mod_notify. count — номер провала
// из attempts; добавляется в detail, чтобы под общим тумблером было видно,
// почему уведомление пришло именно сейчас.
func (b *Bot) notifyCaptchaFail(chatID, targetID int64, kind storage.EventKind, detail string, count int) {
	b.goSafe("notifyCaptchaFail", func() {
		targets := b.captchaNotifyTargets(b.runCtx)
		if count >= 2 {
			seen := make(map[int64]bool, len(targets))
			for _, id := range targets {
				seen[id] = true
			}
			for _, id := range b.modNotifyTargets(b.runCtx) {
				if !seen[id] {
					targets = append(targets, id)
				}
			}
		}
		d := fmt.Sprintf("%s, попытка %d", detail, count)
		b.sendModCard(targets, chatID, targetID, kind, storage.ReasonCaptcha, d)
	})
}

// sendModCard строит и рассылает карточку модерационного события заданным
// получателям. Вызывать из горутины (goSafe) — см. notifyModAction.
func (b *Bot) sendModCard(targets []int64, chatID, targetID int64, kind storage.EventKind, reason string, detail ...string) {
	if len(targets) == 0 {
		return
	}
	action, whoLabel, whyLabel := "👢 Кик", "Кого", "Причина"
	switch kind {
	case storage.EventPass:
		action, whoLabel, whyLabel = "✅ Капча пройдена", "Кто", "Ответ"
	case storage.EventBan, storage.EventSpamBan:
		action = "🚫 Бан"
	}
	why := b.humanReason(reason)
	if len(detail) > 0 && detail[0] != "" {
		d := html.EscapeString(detail[0])
		if why == "" {
			why = d
		} else {
			why += " (" + d + ")"
		}
	}
	infos, _ := b.db.GetUserInfos(b.runCtx, []int64{targetID})
	text := fmt.Sprintf("%s в %s\n%s: %s\n%s: %s",
		action, b.chatLink(b.runCtx, chatID),
		whoLabel, mentionWithUsername(infos, targetID), whyLabel, why)
	for _, ownerID := range targets {
		if _, err := b.api.SendMessage(b.runCtx, tu.Message(tu.ID(ownerID), text).
			WithParseMode(telego.ModeHTML).
			WithLinkPreviewOptions(&telego.LinkPreviewOptions{IsDisabled: true})); err != nil {
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
	case strings.HasPrefix(reason, storage.ReasonReplyApprove):
		raw := strings.TrimPrefix(reason, storage.ReasonReplyApprove)
		adminID, _ := strconv.ParseInt(raw, 10, 64)
		return "одобрен админом " + mentionWithUsername(nameLookup([]int64{adminID}), adminID)
	case strings.HasPrefix(reason, storage.ReasonReplySpam):
		raw := strings.TrimPrefix(reason, storage.ReasonReplySpam)
		adminID, _ := strconv.ParseInt(raw, 10, 64)
		return "спам по решению админа " + mentionWithUsername(nameLookup([]int64{adminID}), adminID)
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
			case strings.HasPrefix(uc.LastReason, storage.ReasonReplyApprove):
				raw := strings.TrimPrefix(uc.LastReason, storage.ReasonReplyApprove)
				if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
					out = append(out, id)
				}
			case strings.HasPrefix(uc.LastReason, storage.ReasonReplySpam):
				raw := strings.TrimPrefix(uc.LastReason, storage.ReasonReplySpam)
				if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
					out = append(out, id)
				}
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
