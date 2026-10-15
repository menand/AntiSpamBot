package bot

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/storage"
)

const (
	// captureBufSize — ёмкость канала захвата. Держит несколько секунд
	// пикового трафика, пока writer-горутина пачкует вставку. При переполнении
	// запись отбрасывается (лучше потерять статистику, чем тормозить модерацию).
	captureBufSize = 2000
	// captureBatchMax — сколько записей складывать в одну транзакцию.
	captureBatchMax = 200
	// captureFlushInt — максимум ожидания неполной пачки перед вставкой.
	// Короткий: текст должен появляться в БД почти в реальном времени.
	captureFlushInt = 2 * time.Second
	// captureReplyMaxRunes — потолок длины сохраняемой цитаты.
	captureReplyMaxRunes = 300
	// captureShutdownTimeout — потолок отсоединённого флаша при остановке.
	captureShutdownTimeout = 3 * time.Second
)

// captureWriter — фоновая горутина захвата: собирает записи из captureCh
// пачками и пишет их в messages одной транзакцией. Запускается только при
// включённом MESSAGE_CAPTURE_ENABLED; иначе канал никто не читает и запись
// в него отбрасывается captureMessage. По ctx.Done дорисовывает остаток
// отсоединённым контекстом (runCtx уже отменён — им писать нельзя) и закрывает
// captureDone, чтобы Run дождался этого ПЕРЕД db.Close().
func (b *Bot) captureWriter(ctx context.Context) {
	defer close(b.captureDone)
	batch := make([]storage.StoredMessage, 0, captureBatchMax)
	flush := func(wctx context.Context) {
		if len(batch) == 0 {
			return
		}
		if err := b.db.AppendMessages(wctx, batch); err != nil {
			// Отменённый контекст (runCtx умер посреди вставки) — пачку
			// СОХРАНЯЕМ: финальный detached-флаш в Done-ветке дочешет её
			// отсоединённым контекстом. Настоящая ошибка БД — логируем и
			// отбрасываем (лучше потерять статистику, чем копить бесконечно).
			if wctx.Err() != nil {
				return
			}
			b.log.Warn("capture: append messages", "err", err, "count", len(batch))
		}
		batch = batch[:0]
	}
	t := time.NewTimer(captureFlushInt)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Последний шанс не потерять накопленное: runCtx мёртв, берём
			// отсоединённый контекст с потолком (паттерн releaseOnAbort).
			shutCtx, cancel := context.WithTimeout(context.Background(), captureShutdownTimeout)
			// Дёргаем и то, что пришло в канал до отмены.
			for drained := true; drained; {
				select {
				case m := <-b.captureCh:
					batch = append(batch, m)
				default:
					drained = false
				}
			}
			flush(shutCtx)
			cancel()
			return
		case m := <-b.captureCh:
			batch = append(batch, m)
			if len(batch) >= captureBatchMax {
				flush(ctx)
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(captureFlushInt)
			}
		case <-t.C:
			flush(ctx)
			t.Reset(captureFlushInt)
		}
	}
}

// waitCaptureDone дожидается, пока captureWriter завершит финальный флаш (по
// ctx.Done) и закроет captureDone. Если захват выключен, горутина не
// запускалась и captureDone никогда не закроется — выходим по таймауту, это
// no-op. Вызывается из Run ПЕРЕД db.Close(), чтобы финальная вставка не упала
// на закрытом соединении.
func (b *Bot) waitCaptureDone() {
	if !b.cfg.MessageCaptureEnabled {
		return
	}
	select {
	case <-b.captureDone:
	case <-time.After(captureShutdownTimeout + time.Second):
		b.log.Warn("capture: writer did not finish in time")
	}
}

// captureMessage сохраняет сообщение в канал захвата. Сначала проверяет флаг
// (дефолт — выключен, тогда вообще не строим запись), потом собирает её и шлёт
// НЕБЛОКИРУЮЩЕ: при переполненном канале (БД отстаёт) отбрасывает — горячий
// путь модерации не должен ждать SQLite. kind — 'text' | 'caption' | 'media' |
// 'edit'.
func (b *Bot) captureMessage(m *telego.Message, kind string) {
	if !b.cfg.MessageCaptureEnabled {
		return
	}
	rec := captureFromMessage(m)
	rec.Kind = kind
	select {
	case b.captureCh <- rec:
	default:
		// Warn, не Debug: это единственный сигнал, что след «кровоточит»
		// (канал переполнен, БД не поспевает). При warn-уровне иначе не видно.
		b.log.Warn("capture: channel full, message dropped",
			"chat", rec.ChatID, "user", rec.UserID)
	}
}

// captureFromMessage собирает самодостаточную запись из telego-сообщения:
// текст или подпись, факт наличия вложения и денормализованная цитата
// (reply-to). Вызывается на горячем пути — только читает поля, без аллокаций
// кроме самой записи и обрезанной цитаты.
//
// Время: для обычного сообщения — m.Date (отправка). Для правки берём
// m.EditDate (если проставлен): m.Date у правки — дата ОРИГИНАЛА, и запись
// легла бы в давно заархивированный день, где следующий прогон архиватора
// перезаписал бы файл дня одними правками, уничтожив оригиналы.
func captureFromMessage(m *telego.Message) storage.StoredMessage {
	ts := m.Date
	if m.EditDate != 0 {
		ts = m.EditDate
	}
	rec := storage.StoredMessage{
		ChatID:  m.Chat.ID,
		UserID:  m.From.ID,
		TGMsgID: int64(m.MessageID),
		TS:      time.Unix(ts, 0),
	}
	rec.Text = m.Text
	if rec.Text == "" {
		rec.Text = m.Caption
	}
	rec.HasMedia = attachmentKindRU(*m) != ""

	// Цитата: тянем автора и текст, чтобы запись читалась без joins.
	if m.ReplyToMessage != nil {
		r := m.ReplyToMessage
		replyMsgID := int64(r.MessageID)
		rec.ReplyMsgID = &replyMsgID
		if r.From != nil {
			uid := r.From.ID
			rec.ReplyUID = &uid
			name := displayName(r.From)
			if name != "" {
				rec.ReplyAuthor = &name
			}
		}
		rt := r.Text
		if rt == "" {
			rt = r.Caption
		}
		if rt != "" {
			rt = truncateRunes(rt, captureReplyMaxRunes)
			rec.ReplyText = &rt
		}
	}
	return rec
}

// captureKindFor отвечает, какой kind ставить для сообщения: обычный текст —
// 'text'; подпись к медиа — 'caption'; медиа без подписи (стикер, фото, опрос)
// — 'media' (текст пустой, has_media=1). Вынесено, чтобы edit-путь не дублировал.
func captureKindFor(m *telego.Message) string {
	switch {
	case m.Text != "":
		return "text"
	case m.Caption != "":
		return "caption"
	default:
		return "media"
	}
}

// displayName — «Имя Фамилия» из telego.User (fallback на username, затем пусто).
func displayName(u *telego.User) string {
	name := u.FirstName
	if u.LastName != "" {
		if name != "" {
			name += " "
		}
		name += u.LastName
	}
	if name == "" && u.Username != "" {
		name = "@" + u.Username
	}
	return name
}

// truncateRunes обрезает строку до max рун, ставя многоточие. Rune-безопасно:
// срез по байтам ломал бы многобайтовый кириллический хвост.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}
