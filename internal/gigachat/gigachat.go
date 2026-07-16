// Package gigachat — минимальный клиент GigaChat API (Сбер) для оценки
// спамности сообщений. Фолбек для internal/groq: у Groq лимиты запросов в
// минуту/сутки, GigaChat подхватывает, когда Groq недоступен. Контракт тот
// же: любая ошибка — это ошибка, НЕ вердикт; вызывающий обязан fail-open.
package gigachat

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/menand/AntiSpamBot/internal/groq"
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
	// Валидность вшитого PEM сторожит TestEmbeddedCertParses — здесь bool
	// некому репортить (у клиента нет логгера).
	pool.AppendCertsFromPEM(trustedRootCA)
	// Clone, а не пустой &http.Transport{}: иначе теряются дефолты —
	// Proxy из окружения (HTTPS_PROXY), таймауты dial/TLS-handshake, HTTP/2.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	return &http.Client{
		// Страховочный транспортный таймаут на случай вызова без дедлайна в
		// ctx; реальный бюджет задаёт вызывающий.
		Timeout:   90 * time.Second,
		Transport: tr,
	}
}

func (c *Client) Enabled() bool { return c != nil && c.authKey != "" }

func (c *Client) Model() string { return c.model }

// errUnauthorized — chat ответил 401: токен отозван раньше expires_at.
var errUnauthorized = errors.New("gigachat: unauthorized")

// IsSpam шлёт факты о сообщении с конструкторским системным промптом
// (общим с groq-клиентом).
func (c *Client) IsSpam(ctx context.Context, facts string) (bool, error) {
	return c.Classify(ctx, c.systemPrompt, facts)
}

// Classify шлёт факты с произвольным системным промптом и возвращает
// бинарный вердикт (true = спам). На 401 от chat-эндпоинта делает один
// принудительный рефреш токена и повторяет — устойчивость к протухшему
// токену общая для всех промптов.
func (c *Client) Classify(ctx context.Context, system, facts string) (bool, error) {
	token, err := c.getToken(ctx, false)
	if err != nil {
		return false, err
	}
	content, err := c.chat(ctx, token, system, facts)
	if errors.Is(err, errUnauthorized) {
		token, err = c.getToken(ctx, true)
		if err != nil {
			return false, err
		}
		content, err = c.chat(ctx, token, system, facts)
	}
	if err != nil {
		return false, err
	}
	return groq.ParseVerdict(content)
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
	req.Header.Set("RqUID", uuid.NewString())

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
	if tr.ExpiresAt > 0 {
		c.expiresAt = time.UnixMilli(tr.ExpiresAt).Add(-tokenSafetyMargin)
	} else {
		// expires_at не пришёл (изменение API?) — не считать свежий токен
		// протухшим, иначе каждый вызов делает полный OAuth. Док обещает
		// 30 минут жизни; берём с запасом.
		c.expiresAt = time.Now().Add(25 * time.Minute)
	}
	return c.token, nil
}

type chatRequest struct {
	Model string `json:"model"`
	// Temperature 0 — как у Groq-клиента: классификатору нужен
	// воспроизводимый вердикт, а не «сбалансированный ответ».
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

// chat выполняет один запрос к chat/completions и возвращает сырой текст
// ответа модели (вердикт из него достаёт groq.ParseVerdict — общий для обоих
// провайдеров). 401 возвращается как errUnauthorized, чтобы вызывающий мог
// сделать рефреш токена.
func (c *Client) chat(ctx context.Context, token, system, facts string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Temperature: 0,
		MaxTokens:   64,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: facts},
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal gigachat request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build gigachat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("gigachat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read gigachat response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("%w: %.200s", errUnauthorized, raw)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gigachat status %d: %.200s", resp.StatusCode, raw)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("parse gigachat envelope: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("gigachat: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
}
