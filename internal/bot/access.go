package bot

import (
	"context"
	"time"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// userChats возвращает подмножество известных чатов, которыми юзер может
// управлять: для OWNER_IDS — все, для остальных — только чаты, где юзер
// админ/создатель. Проверки админства идут через 6-часовой кэш, поэтому
// только первое открытие меню стоит не-владельцу N API-запросов при N
// известных чатах. Чаты, не подтверждённые владельцем (pending/rejected),
// не показываем: решения по ним принимаются ЛС-вопросом, а меню/статистика
// их засветили бы пустыми данными.
func (b *Bot) userChats(ctx context.Context, userID int64) ([]storage.ChatInfo, error) {
	all, err := b.db.ListChats(ctx)
	if err != nil {
		return nil, err
	}
	serviceable := make([]storage.ChatInfo, 0, len(all))
	for _, c := range all {
		// Тот же гейт, что у кнопок меню: чат вне ALLOWED_CHATS не должен
		// висеть в списке с мёртвыми кнопками и попадать в отчёты.
		if b.chatServiceable(c.ChatID) {
			serviceable = append(serviceable, c)
		}
	}
	if b.isOwner(userID) {
		return serviceable, nil
	}
	out := make([]storage.ChatInfo, 0, len(serviceable))
	for _, c := range serviceable {
		if b.isChatAdminCached(ctx, c.ChatID, userID) {
			out = append(out, c)
		}
	}
	return out, nil
}

// canManageChat отвечает, может ли юзер смотреть статистику и крутить
// настройки конкретного чата: либо владелец бота, либо админ/создатель чата.
// Статус админа берётся из кэша (инвалидируется на каждом chat_member; в
// чатах, где бот сам не админ, Telegram таких событий не шлёт — поэтому у
// негативных ответов короткий TTL) — тот же класс устаревания, что уже
// принят для золотого голоса в спам-голосовании.
func (b *Bot) canManageChat(ctx context.Context, userID, chatID int64) bool {
	return b.isOwner(userID) || b.isChatAdminCached(ctx, chatID, userID)
}

// chatSettings загружает пер-чатовые настройки для РЕЗОЛВИНГА (параметры
// капчи, приветствие, пороги антиспама): ошибка логируется здесь, а
// возвращённая структура всё равно несёт дефолты (это гарантирует
// GetChatSettings), так что вызывающие резолвят по ней безусловно. НЕ
// использовать для read-modify-write (тогглы настроек) и для отрисовки
// экрана настроек — те обязаны звать GetChatSettings напрямую и прерываться
// на ошибке, иначе запишут/покажут инверсию дефолта вместо сохранённого
// значения.
func (b *Bot) chatSettings(ctx context.Context, chatID int64) storage.ChatSettings {
	s, err := b.db.GetChatSettings(ctx, chatID)
	if err != nil {
		b.log.Warn("get chat settings", "err", err, "chat", chatID)
	}
	return s
}

// effectiveMaxAttempts резолвит число попыток: пер-чатовый override, если
// задан, иначе глобальный дефолт из конфига.
func (b *Bot) effectiveMaxAttempts(s storage.ChatSettings) int {
	if s.MaxAttempts.Valid {
		return int(s.MaxAttempts.Int64)
	}
	return b.cfg.MaxAttempts
}

// effectiveCaptchaTimeout резолвит таймаут капчи: пер-чатовый override, если
// задан, иначе глобальный дефолт.
func (b *Bot) effectiveCaptchaTimeout(s storage.ChatSettings) time.Duration {
	if s.CaptchaTimeoutSeconds.Valid {
		return time.Duration(s.CaptchaTimeoutSeconds.Int64) * time.Second
	}
	return b.cfg.CaptchaTimeout
}

// effectiveDailyHour резолвит UTC-час ежедневной сводки: пер-чатовый
// override, если задан, иначе глобальный дефолт. (Сам цикл сводок резолвит
// это в SQL через COALESCE — этот хелпер для отображения на стороне Go.)
func (b *Bot) effectiveDailyHour(s storage.ChatSettings) int {
	if s.DailyStatsUTCHour.Valid {
		return int(s.DailyStatsUTCHour.Int64)
	}
	return b.cfg.DailyStatsUTCHour
}

// effectiveCaptchaMode резолвит вид капчи. Неизвестные значения из БД
// (будущие / битые) откатываются к ModeCircles.
func effectiveCaptchaMode(s storage.ChatSettings) captcha.Mode {
	if !s.CaptchaMode.Valid {
		return captcha.ModeCircles
	}
	switch captcha.Mode(s.CaptchaMode.String) {
	case captcha.ModeEmoji:
		return captcha.ModeEmoji
	case captcha.ModeImage:
		return captcha.ModeImage
	default:
		return captcha.ModeCircles
	}
}
