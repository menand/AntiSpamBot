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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
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

// SpamProbability шлёт факты о сообщении и возвращает 0..100. На 401 от
// chat-эндпоинта делает один принудительный рефреш токена и повторяет.
func (c *Client) SpamProbability(ctx context.Context, facts string) (int, error) {
	token, err := c.getToken(ctx, false)
	if err != nil {
		return 0, err
	}
	prob, err := c.chat(ctx, token, facts)
	if errors.Is(err, errUnauthorized) {
		token, err = c.getToken(ctx, true)
		if err != nil {
			return 0, err
		}
		prob, err = c.chat(ctx, token, facts)
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

// chat выполняет один запрос к chat/completions. 401 возвращается как
// errUnauthorized, чтобы вызывающий мог сделать рефреш токена.
func (c *Client) chat(ctx context.Context, token, facts string) (int, error) {
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Temperature: 0,
		MaxTokens:   64,
		Messages: []chatMessage{
			{Role: "system", Content: c.systemPrompt},
			{Role: "user", Content: facts},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("marshal gigachat request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatEndpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build gigachat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gigachat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("read gigachat response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return 0, fmt.Errorf("%w: %.200s", errUnauthorized, raw)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("gigachat status %d: %.200s", resp.StatusCode, raw)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return 0, fmt.Errorf("parse gigachat envelope: %w", err)
	}
	if len(cr.Choices) == 0 {
		return 0, fmt.Errorf("gigachat: empty choices")
	}
	return parseProbability(cr.Choices[0].Message.Content)
}

var (
	probObjRe = regexp.MustCompile(`\{[^{}]*"spam_probability"[^{}]*\}`)
	probNumRe = regexp.MustCompile(`\d+(?:[.,]\d+)?|[.,]\d+`)
)

// parseProbability достаёт spam_probability из ответа модели. В отличие от
// Groq, GigaChat не гарантирует json-режим: модель может обернуть объект в
// код-блок или текст. Ступени: JSON-объект с ключом (в том числе весь ответ
// целиком) → единственное однозначное число в тексте. Неоднозначность — это
// ошибка, а не догадка: fail-open с Warn честнее ложного вердикта.
func parseProbability(content string) (int, error) {
	if obj := probObjRe.FindString(content); obj != "" {
		// Указатель, чтобы отличить {"spam_probability": 0} от объекта без
		// ключа/с опечаткой — второй не должен тихо стать нулём.
		var v struct {
			SpamProbability *float64 `json:"spam_probability"`
		}
		if json.Unmarshal([]byte(obj), &v) == nil && v.SpamProbability != nil {
			return clampScale(*v.SpamProbability), nil
		}
	}
	// Фолбек: во всём ответе ровно одно число, и оно похоже на вероятность.
	// «Вероятность спама: 61%» → 61; «по шкале от 0 до 100 даю 95» — три
	// числа, угадывать нельзя.
	nums := probNumRe.FindAllString(content, -1)
	if len(nums) == 1 {
		if f, err := strconv.ParseFloat(strings.Replace("0"+nums[0], ",", ".", 1), 64); err == nil && f <= 100 {
			return clampScale(f), nil
		}
	}
	return 0, fmt.Errorf("gigachat: no unambiguous spam probability in %q", content)
}

// clampScale нормализует значение вероятности: дробь 0..1 (модель ответила
// «0.95») переводится в проценты, результат зажимается в 0..100. Ровно 1 НЕ
// масштабируется: промпт требует целое 0..100, и «1» — это честный 1%;
// трактовка его как 100% давала бы ложную плашку на каждом безобидном
// сообщении. Аномальная «максимальная уверенность» на шкале 0..1 при этом
// прочтётся как 1% — осознанный проигрыш редкого кейса частому.
func clampScale(v float64) int {
	if v > 0 && v < 1 {
		v *= 100
	}
	return min(100, max(0, int(v)))
}
