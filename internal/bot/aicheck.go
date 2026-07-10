package bot

import (
	"context"
	"fmt"
	"html"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// spamClassifier — общий срез API обоих LLM-клиентов (groq, gigachat),
// достаточный для диагностики. Оба удовлетворяют ему как есть.
type spamClassifier interface {
	Enabled() bool
	Model() string
	SpamProbability(ctx context.Context, facts string) (int, error)
}

// aiCheckFacts — безобидный фиксированный вход: реальный вызов проверяет всю
// связку разом (ключ, сеть/TLS, модель, парсер ответа).
const aiCheckFacts = "Автор: Проверка связи, всего сообщений: 100.\n" +
	"Текст сообщения:\nПривет! Это тестовое сообщение для проверки подключения."

const aiCheckTimeout = 15 * time.Second

// runAICheck опрашивает каждого провайдера ОТДЕЛЬНО (не через classifySpam —
// его фолбек замаскировал бы умершего первичного) и редактирует ранее
// отправленное «⏳…» сообщение итогом. Запускается горутиной из меню.
func (b *Bot) runAICheck(dmChatID int64, msgID int) {
	providers := []struct {
		name string
		c    spamClassifier
	}{
		{"groq", b.groqc},
		{"gigachat", b.gigac},
	}

	lines := make([]string, len(providers))
	done := make(chan struct{}, len(providers))
	for i, p := range providers {
		go func(i int, name string, c spamClassifier) {
			defer func() { done <- struct{}{} }()
			if !c.Enabled() {
				lines[i] = fmt.Sprintf("➖ %s — ключ не задан", name)
				return
			}
			ctx, cancel := context.WithTimeout(b.runCtx, aiCheckTimeout)
			defer cancel()
			start := time.Now()
			prob, err := c.SpamProbability(ctx, aiCheckFacts)
			lines[i] = formatProviderCheck(name, c.Model(), prob, time.Since(start), err)
		}(i, p.name, p.c)
	}
	for range providers {
		<-done
	}

	text := "🔌 <b>Проверка ИИ-провайдеров</b>\n\n"
	for _, l := range lines {
		text += l + "\n"
	}
	if _, err := b.api.EditMessageText(b.runCtx, &telego.EditMessageTextParams{
		ChatID:    tu.ID(dmChatID),
		MessageID: msgID,
		Text:      text,
		ParseMode: telego.ModeHTML,
	}); err != nil {
		b.log.Warn("edit ai check result", "err", err, "chat", dmChatID)
	}
}

// formatProviderCheck рендерит одну строку итога. Текст ошибки экранируется и
// режется: тела ответов API бывают длинными и содержат разметку.
func formatProviderCheck(name, model string, prob int, elapsed time.Duration, err error) string {
	if err != nil {
		return fmt.Sprintf("❌ %s (%s) — ошибка за %.1f с: <code>%s</code>",
			name, model, elapsed.Seconds(),
			html.EscapeString(truncateLabel(err.Error(), 200)))
	}
	return fmt.Sprintf("✅ %s (%s) — коннект есть: %.1f с, вердикт %d%%",
		name, model, elapsed.Seconds(), prob)
}
