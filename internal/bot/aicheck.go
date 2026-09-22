package bot

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/groq"
)

// llmClassifier — общий срез API всех LLM-клиентов (groq, gemini, gigachat):
// единое объявление для цепочки фолбеков classifyVerdict и диагностики
// aiCheck. Все клиенты удовлетворяют ему как есть (IsSpam — делегат Classify).
type llmClassifier interface {
	Enabled() bool
	Model() string
	Classify(ctx context.Context, system, facts string) (bool, error)
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
		c    llmClassifier
	}{
		{"groq", b.groqc},
		{"gemini", b.gemic},
		{"gigachat", b.gigac},
	}

	lines := make([]string, len(providers))
	done := make(chan struct{}, len(providers))
	for i, p := range providers {
		b.goSafe("aiCheck:"+p.name, func() {
			defer func() { done <- struct{}{} }()
			if !p.c.Enabled() {
				lines[i] = fmt.Sprintf("➖ %s — ключ не задан", p.name)
				return
			}
			ctx, cancel := context.WithTimeout(b.runCtx, aiCheckTimeout)
			defer cancel()
			start := time.Now()
			spam, err := p.c.Classify(ctx, groq.SystemPrompt, aiCheckFacts)
			lines[i] = formatProviderCheck(p.name, p.c.Model(), spam, time.Since(start), err)
		})
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
func formatProviderCheck(name, model string, spam bool, elapsed time.Duration, err error) string {
	if err != nil {
		tail := html.EscapeString(truncateLabel(err.Error(), 200))
		if ru := friendlyProviderErr(err); ru != "" {
			return fmt.Sprintf("❌ %s (%s) — ошибка за %.1f с: %s — <code>%s</code>",
				name, model, elapsed.Seconds(), ru, tail)
		}
		return fmt.Sprintf("❌ %s (%s) — ошибка за %.1f с: <code>%s</code>",
			name, model, elapsed.Seconds(), tail)
	}
	verdict := "не спам"
	if spam {
		verdict = "спам"
	}
	return fmt.Sprintf("✅ %s (%s) — коннект есть: %.1f с, вердикт: %s",
		name, model, elapsed.Seconds(), verdict)
}

// friendlyProviderErr — короткая русская категория типовой сетевой/HTTP-ошибки
// для карточки menu:aicheck (пользовательский текст должен быть на русском);
// непонятное возвращаем пустым — теххвост <code>…</code> остаётся как есть.
func friendlyProviderErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "deadline exceeded"), strings.Contains(s, "timeout"):
		return "таймаут"
	case strings.Contains(s, "connection refused"), strings.Contains(s, "no such host"),
		strings.Contains(s, "dial tcp"):
		return "нет соединения"
	case strings.Contains(s, "401"), strings.Contains(s, "403"):
		return "ключ отклонён"
	case strings.Contains(s, "429"):
		return "лимит запросов"
	default:
		return ""
	}
}
