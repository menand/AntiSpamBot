// Package gigachat — минимальный клиент GigaChat API (Сбер) для оценки
// спамности сообщений. Фолбек для internal/groq: у Groq лимиты запросов в
// минуту/сутки, GigaChat подхватывает, когда Groq недоступен. Контракт тот
// же: любая ошибка — это ошибка, НЕ вердикт; вызывающий обязан fail-open.
package gigachat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultOAuthEndpoint = "https://ngw.devices.sberbank.ru:9443/api/v2/oauth"
	defaultChatEndpoint  = "https://gigachat.devices.sberbank.ru/api/v1/chat/completions"

	// DefaultScope — персональный аккаунт (бесплатные токены).
	DefaultScope = "GIGACHAT_API_PERS"
	// DefaultModel — Lite-модель; для бинарной классификации спама хватает.
	// Переопределяется env GIGACHAT_MODEL.
	DefaultModel = "GigaChat"

	// tokenSafetyMargin — обновляем access_token чуть раньше его expires_at,
	// чтобы не поймать 401 на границе 30-минутного окна.
	tokenSafetyMargin = time.Minute
)

// Хосты Сбера работают под сертификатом НУЦ Минцифры, которого нет в
// системных сторах за пределами РФ — без него TLS-хендшейк падает. Корневой
// сертификат публичный (gosuslugi.ru/crt), вшиваем его в бинарь.
//
//go:embed russian_trusted_root_ca.pem
var trustedRootCA []byte

type Client struct {
	authKey      string // base64(client_id:secret) — Authorization: Basic
	scope        string
	model        string
	systemPrompt string

	oauthEndpoint string
	chatEndpoint  string
	http          *http.Client

	// Кэш OAuth-токена (живёт ~30 минут). Мьютекс держится и на время
	// самого OAuth-запроса — заодно защищает от стада рефрешей.
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// New возвращает клиент; пустой authKey допустим — Enabled() будет false.
// systemPrompt передаётся снаружи (общий с groq-клиентом).
func New(authKey, scope, model, systemPrompt string) *Client {
	if scope == "" {
		scope = DefaultScope
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		authKey:       authKey,
		scope:         scope,
		model:         model,
		systemPrompt:  systemPrompt,
		oauthEndpoint: defaultOAuthEndpoint,
		chatEndpoint:  defaultChatEndpoint,
		http:          newHTTPClient(),
	}
}

func newHTTPClient() *http.Client {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(trustedRootCA)
	return &http.Client{
		// Страховочный транспортный таймаут на случай вызова без дедлайна в
		// ctx; реальный бюджет задаёт вызывающий.
		Timeout: 90 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
}

func (c *Client) Enabled() bool { return c != nil && c.authKey != "" }

func (c *Client) Model() string { return c.model }

// SpamProbability шлёт факты о сообщении и возвращает 0..100. На 401 от
// chat-эндпоинта делает один принудительный рефреш токена и повторяет.
func (c *Client) SpamProbability(ctx context.Context, facts string) (int, error) {
	token, err := c.getToken(ctx, false)
	if err != nil {
		return 0, err
	}
	prob, status, err := c.chat(ctx, token, facts)
	if status == http.StatusUnauthorized {
		// Токен мог быть отозван раньше expires_at.
		token, err = c.getToken(ctx, true)
		if err != nil {
			return 0, err
		}
		prob, _, err = c.chat(ctx, token, facts)
	}
	return prob, err
}

// getToken возвращает валидный access_token: из кэша, если не протух, иначе
// новым OAuth-запросом. force игнорирует кэш (после 401 от chat).
func (c *Client) getToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && time.Now().Before(c.expiresAt) {
		return c.token, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.oauthEndpoint,
		strings.NewReader(url.Values{"scope": {c.scope}}.Encode()))
	if err != nil {
		return "", fmt.Errorf("build gigachat oauth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+c.authKey)
	req.Header.Set("RqUID", newRqUID())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("gigachat oauth: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read gigachat oauth response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gigachat oauth status %d: %.200s", resp.StatusCode, raw)
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"` // unix-миллисекунды
	}
	if err := json.Unmarshal(raw, &tr); err != nil {
		return "", fmt.Errorf("parse gigachat oauth response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("gigachat oauth: empty access_token")
	}
	c.token = tr.AccessToken
	c.expiresAt = time.UnixMilli(tr.ExpiresAt).Add(-tokenSafetyMargin)
	return c.token, nil
}

type chatRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	Messages  []chatMessage `json:"messages"`
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

// chat выполняет один запрос к chat/completions. Возвращает HTTP-статус
// отдельно, чтобы вызывающий мог отличить 401 (протухший токен) от прочего.
func (c *Client) chat(ctx context.Context, token, facts string) (int, int, error) {
	body, err := json.Marshal(chatRequest{
		Model:     c.model,
		MaxTokens: 64,
		Messages: []chatMessage{
			{Role: "system", Content: c.systemPrompt},
			{Role: "user", Content: facts},
		},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("marshal gigachat request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatEndpoint, bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("build gigachat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("gigachat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, resp.StatusCode, fmt.Errorf("read gigachat response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, resp.StatusCode, fmt.Errorf("gigachat status %d: %.200s", resp.StatusCode, raw)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return 0, resp.StatusCode, fmt.Errorf("parse gigachat envelope: %w", err)
	}
	if len(cr.Choices) == 0 {
		return 0, resp.StatusCode, fmt.Errorf("gigachat: empty choices")
	}
	p, err := parseProbability(cr.Choices[0].Message.Content)
	if err != nil {
		return 0, resp.StatusCode, err
	}
	return p, resp.StatusCode, nil
}

var (
	probObjRe = regexp.MustCompile(`\{[^{}]*"spam_probability"[^{}]*\}`)
	probNumRe = regexp.MustCompile(`\d{1,3}`)
)

// parseProbability достаёт spam_probability из ответа модели. В отличие от
// Groq, GigaChat не гарантирует json-режим: модель может обернуть объект в
// код-блок или текст. Ступени: чистый JSON → JSON-объект внутри текста →
// первое число в строке.
func parseProbability(content string) (int, error) {
	try := func(s string) (int, bool) {
		var v struct {
			SpamProbability float64 `json:"spam_probability"`
		}
		if json.Unmarshal([]byte(s), &v) == nil {
			return clamp(int(v.SpamProbability)), true
		}
		return 0, false
	}
	if p, ok := try(content); ok {
		return p, nil
	}
	if obj := probObjRe.FindString(content); obj != "" {
		if p, ok := try(obj); ok {
			return p, nil
		}
	}
	if m := probNumRe.FindString(content); m != "" {
		if n, err := strconv.Atoi(m); err == nil {
			return clamp(n), nil
		}
	}
	return 0, fmt.Errorf("gigachat: no spam probability in %q", content)
}

func clamp(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// newRqUID генерирует UUIDv4 для обязательного заголовка RqUID OAuth-запроса.
func newRqUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand на практике не падает; фиксированный валидный UUID —
		// достаточный запасной выход для необязательного к уникальности поля.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // версия 4
	b[8] = (b[8] & 0x3f) | 0x80 // вариант RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
