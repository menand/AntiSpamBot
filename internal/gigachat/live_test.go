package gigachat

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/menand/AntiSpamBot/internal/groq"
)

// TestLiveSpamProbability бьёт в настоящий GigaChat API — прогоняется вручную:
//
//	GIGACHAT_AUTH_KEY=... go test -run TestLive ./internal/gigachat -v
//
// Без ключа (CI, обычный прогон) тест скипается. Проверяет всю прод-связку:
// TLS с сертификатом НУЦ Минцифры, OAuth, chat и робастный парсер — с тем же
// системным промптом, что использует бот. Ассерты нарочно мягкие.
func TestLiveSpamProbability(t *testing.T) {
	key := os.Getenv("GIGACHAT_AUTH_KEY")
	if key == "" {
		t.Skip("GIGACHAT_AUTH_KEY не задан — живой тест пропущен")
	}
	c := New(key, os.Getenv("GIGACHAT_SCOPE"), os.Getenv("GIGACHAT_MODEL"), groq.SystemPrompt)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cases := []struct {
		name     string
		facts    string
		min, max int
	}{
		{
			name: "явный спам: заработок",
			facts: "Автор: Мария (@maria_zarabotok), в чате 2 минуты, всего сообщений: 1.\n" +
				"Текст сообщения:\nЗaрaбoтok от 800$ в нeдeлю! Удалённо, 2-3 часа в день. Пиши + в личку",
			min: 60, max: 100,
		},
		{
			name: "не спам: вопрос о тренировке",
			facts: "Автор: Ирина (@irina_run), в чате 3 дня, всего сообщений: 4.\n" +
				"Текст сообщения:\nКто завтра на паркран к 9:00? Говорят жарко будет, берите воду",
			min: 0, max: 40,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := c.SpamProbability(ctx, tc.facts)
			if err != nil {
				t.Fatalf("gigachat error: %v", err)
			}
			t.Logf("prob=%d (ожидание %d..%d)", p, tc.min, tc.max)
			if p < tc.min || p > tc.max {
				t.Errorf("вероятность %d вне ожидаемого диапазона [%d, %d]", p, tc.min, tc.max)
			}
		})
	}
}
