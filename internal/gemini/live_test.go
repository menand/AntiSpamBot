package gemini

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveIsSpam бьёт в настоящий Gemini API — прогоняется вручную:
//
//	GEMINI_API_KEY=... go test -run TestLive ./internal/gemini -v
//
// Без ключа (CI, обычный прогон) тест скипается. Кейсы — заведомо
// однозначные (пересекаются с groq-тестом).
func TestLiveIsSpam(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY не задан — живой тест пропущен")
	}
	c := New(key, os.Getenv("GEMINI_MODEL"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cases := []struct {
		name  string
		facts string
		want  bool
	}{
		{
			name: "явный спам: заработок",
			facts: "Автор: Мария (@maria_zarabotok), в чате 2 минуты, всего сообщений: 1.\n" +
				"Текст сообщения:\nЗaрaбoтok от 800$ в нeдeлю! Удалённо, 2-3 часа в день. Пиши + в личку",
			want: true,
		},
		{
			name: "явный спам: вербовка в крипто-команду",
			facts: "Автор: Alex (@alex_crypto), в чате меньше минуты, всего сообщений: 1.\n" +
				"Текст сообщения:\nИщу партнёров на криптобиржу, обучение бесплатно, беру небольшой % от прибыли. Без опыта, час в день. Пишите в личку",
			want: true,
		},
		{
			name: "не спам: вопрос о тренировке",
			facts: "Автор: Ирина (@irina_run), в чате 3 дня, всего сообщений: 4.\n" +
				"Текст сообщения:\nКто завтра на паркран к 9:00? Говорят жарко будет, берите воду",
			want: false,
		},
		{
			name: "не спам: приветствие новичка",
			facts: "Автор: Пётр, в чате 5 минут, всего сообщений: 1.\n" +
				"Текст сообщения:\nВсем привет! Только вступил, подскажите, где обычно старт по субботам?",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spam, err := c.IsSpam(ctx, tc.facts)
			if err != nil {
				t.Fatalf("gemini error: %v", err)
			}
			t.Logf("spam=%v (ожидание %v)", spam, tc.want)
			if spam != tc.want {
				t.Errorf("вердикт %v, ожидался %v", spam, tc.want)
			}
		})
	}
}
