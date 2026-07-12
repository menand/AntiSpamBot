package bot

import (
	"context"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// rememberChat апсертит строку реестра чатов, пропуская запись в БД, когда
// закэшированное значение не изменилось. Вызывается на каждом групповом
// сообщении — без кэша это была бы лишняя SQLite-запись на каждое. Кэш
// обновляется только после успешной записи, чтобы неудачный upsert
// повторился на следующем сообщении. Неограниченный размер — осознанно:
// запись ~100 байт, а пространство ключей — множество чатов бота.
func (b *Bot) rememberChat(ctx context.Context, info storage.ChatInfo) {
	b.cacheMu.Lock()
	cached, ok := b.chatCache[info.ChatID]
	b.cacheMu.Unlock()
	if ok && cached == info {
		return
	}
	if err := b.db.RememberChat(ctx, info); err != nil {
		b.log.Warn("remember chat", "err", err, "chat", info.ChatID)
		return
	}
	b.cacheMu.Lock()
	b.chatCache[info.ChatID] = info
	b.cacheMu.Unlock()
}

// rememberUser — тот же write-through-паттерн для display-имён юзеров.
// Память растёт с числом уникальных активных юзеров за время жизни
// процесса — для масштаба группового бота приемлемо.
func (b *Bot) rememberUser(ctx context.Context, info storage.UserInfo) {
	b.cacheMu.Lock()
	cached, ok := b.userCache[info.UserID]
	b.cacheMu.Unlock()
	if ok && cached == info {
		return
	}
	if err := b.db.RememberUser(ctx, info); err != nil {
		b.log.Warn("remember user", "err", err, "user", info.UserID)
		return
	}
	b.cacheMu.Lock()
	b.userCache[info.UserID] = info
	b.cacheMu.Unlock()
}
