// Package gemini — минимальный клиент Gemini API (Google) для оценки
// спамности сообщений через OpenAI-совместимый эндпоинт Google. Средний
// фолбек между Groq (первичный) и GigaChat (запасной): бесплатный тир Google
// большой и стабильный, русский у flash-моделей хороший. Контракт тот же:
// любая ошибка — это ошибка, НЕ вердикт; вызывающий обязан fail-open.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/menand/AntiSpamBot/internal/groq"
)

const (
	// defaultEndpoint — OpenAI-совместимая обёртка над Gemini API: тот же
	// chat/completions JSON, что у Groq, отличается только базой и ключом.
	defaultEndpoint = "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"

	// DefaultModel — стабильная flash-lite-модель с бесплатным тиром; для бинарной
	// классификации спама хватает. Переопределяется env GEMINI_MODEL.
	//
	// Почему НЕ gemini-2.5-flash: это reasoning-модель — перед ответом она
	// «думает» (thinking tokens), и при скромном max_tokens весь бюджет уходил в
	// мышление, а content возвращался пустым → ParseVerdict("") падал. Мышление
	// 2.5-поколения можно попытаться отключить reasoning_effort=none, но это не
	// гарантирует непустой ответ (2.5-flash-lite пустовала и с ним); при этом
	// 3.x-модели reasoning_effort отвергают вообще (HTTP 400 INVALID_ARGUMENT).
	// flash-lite 3.x отвечает вердиктом без всяких хаков — поэтому здесь его
	// НЕ слать.
	DefaultModel = "gemini-3.5-flash-lite"
)

type Client struct {
	apiKey   string
	model    string
	endpoint string
	http     *http.Client
}

// New возвращает клиент; пустой apiKey допустим — Enabled() будет false.
func New(apiKey, model string) *Client {
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		apiKey:   apiKey,
		model:    model,
		endpoint: defaultEndpoint,
		// Страховочный транспортный таймаут на случай вызова без дедлайна в
		// ctx; реальный бюджет задаёт вызывающий (см. groq.Client).
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

func (c *Client) Enabled() bool { return c != nil && c.apiKey != "" }

func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model       string        `json:"model"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Messages    []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// IsSpam шлёт факты о сообщении с базовым спам-промптом (общим с groq).
func (c *Client) IsSpam(ctx context.Context, facts string) (bool, error) {
	return c.Classify(ctx, groq.SystemPrompt, facts)
}

// Classify шлёт факты с произвольным системным промптом и возвращает
// бинарный вердикт (true = спам).
func (c *Client) Classify(ctx context.Context, system, facts string) (bool, error) {
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Temperature: 0,
		// 256, а не 64 как у Groq: reasoning-модели Gemini тратят токены на
		// «мышление», и малого лимита хватало только на него — content выходил
		// пустым. Запас сверху вердикта безвреден.
		MaxTokens: 256,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: facts},
		},
	})
	if err != nil {
		return false, fmt.Errorf("marshal gemini request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body)) //nolint:gosec // endpoint from env var, not user input
	if err != nil {
		return false, fmt.Errorf("build gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req) //nolint:gosec // endpoint from env var
	if err != nil {
		return false, fmt.Errorf("gemini request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, fmt.Errorf("read gemini response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("gemini status %d: %.200s", resp.StatusCode, raw)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return false, fmt.Errorf("parse gemini envelope: %w", err)
	}
	if len(cr.Choices) == 0 {
		return false, fmt.Errorf("gemini: empty choices")
	}
	return groq.ParseVerdict(cr.Choices[0].Message.Content)
}
