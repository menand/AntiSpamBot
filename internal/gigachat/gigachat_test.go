package gigachat

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var rqUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// testServer поднимает мок обоих эндпоинтов. oauthCalls/chatCalls — счётчики
// для проверки кэширования токена; failNextChat — статус одного следующего
// chat-ответа (0 = 200).
type testServer struct {
	*httptest.Server
	oauthCalls   atomic.Int32
	chatCalls    atomic.Int32
	tokenTTL     time.Duration
	chatContent  string
	failNextChat atomic.Int32
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{tokenTTL: 30 * time.Minute, chatContent: `{"spam_probability": 93}`}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth", func(w http.ResponseWriter, r *http.Request) {
		ts.oauthCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Basic test-auth-key" {
			t.Errorf("oauth Authorization = %q", got)
		}
		if got := r.Header.Get("RqUID"); !rqUIDRe.MatchString(got) {
			t.Errorf("RqUID %q is not a UUIDv4", got)
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("scope") != "GIGACHAT_API_PERS" {
			t.Errorf("oauth scope = %q (err=%v)", r.PostForm.Get("scope"), err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("tok-%d", ts.oauthCalls.Load()),
			"expires_at":   time.Now().Add(ts.tokenTTL).UnixMilli(),
		})
	})
	mux.HandleFunc("/chat", func(w http.ResponseWriter, r *http.Request) {
		ts.chatCalls.Add(1)
		if status := ts.failNextChat.Swap(0); status != 0 {
			w.WriteHeader(int(status))
			return
		}
		if got := r.Header.Get("Authorization"); got == "Bearer " || got == "" {
			t.Errorf("chat has no bearer token: %q", got)
		}
		var req struct {
			Temperature *float64 `json:"temperature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Temperature == nil || *req.Temperature != 0 {
			t.Errorf("chat request must pin temperature=0 (err=%v, got %v)", err, req.Temperature)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": ts.chatContent}}},
		})
	})
	ts.Server = httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newTestClient(ts *testServer) *Client {
	c := New("test-auth-key", "", "", "SYSTEM PROMPT")
	c.oauthEndpoint = ts.URL + "/oauth"
	c.chatEndpoint = ts.URL + "/chat"
	return c
}

func TestSpamProbabilityOK(t *testing.T) {
	ts := newTestServer(t)
	c := newTestClient(ts)
	p, err := c.SpamProbability(context.Background(), "текст")
	if err != nil || p != 93 {
		t.Fatalf("want 93, got %d err=%v", p, err)
	}
	if ts.oauthCalls.Load() != 1 || ts.chatCalls.Load() != 1 {
		t.Fatalf("oauth=%d chat=%d, want 1/1", ts.oauthCalls.Load(), ts.chatCalls.Load())
	}
}

func TestTokenCachedBetweenCalls(t *testing.T) {
	ts := newTestServer(t)
	c := newTestClient(ts)
	for i := 0; i < 3; i++ {
		if _, err := c.SpamProbability(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
	}
	if ts.oauthCalls.Load() != 1 {
		t.Fatalf("token must be cached: oauth called %d times", ts.oauthCalls.Load())
	}
	if ts.chatCalls.Load() != 3 {
		t.Fatalf("chat calls = %d, want 3", ts.chatCalls.Load())
	}
}

func TestTokenRefetchedWhenExpired(t *testing.T) {
	ts := newTestServer(t)
	// TTL меньше страховочной минуты — токен «протухает» сразу.
	ts.tokenTTL = time.Second
	c := newTestClient(ts)
	_, _ = c.SpamProbability(context.Background(), "x")
	_, _ = c.SpamProbability(context.Background(), "x")
	if ts.oauthCalls.Load() != 2 {
		t.Fatalf("expired token must be refetched: oauth called %d times", ts.oauthCalls.Load())
	}
}

func TestTokenRefreshOn401(t *testing.T) {
	ts := newTestServer(t)
	ts.failNextChat.Store(http.StatusUnauthorized) // первый chat — 401
	c := newTestClient(ts)
	p, err := c.SpamProbability(context.Background(), "x")
	if err != nil || p != 93 {
		t.Fatalf("after 401+refresh want 93, got %d err=%v", p, err)
	}
	if ts.oauthCalls.Load() != 2 || ts.chatCalls.Load() != 2 {
		t.Fatalf("oauth=%d chat=%d, want 2/2 (forced refresh + retry)",
			ts.oauthCalls.Load(), ts.chatCalls.Load())
	}
}

func TestOAuthErrorPropagates(t *testing.T) {
	ts := newTestServer(t)
	c := newTestClient(ts)
	c.oauthEndpoint = ts.URL + "/nope"
	if _, err := c.SpamProbability(context.Background(), "x"); err == nil {
		t.Fatal("oauth failure must surface as error (fail-open at caller)")
	}
}

func TestChatErrorPropagates(t *testing.T) {
	ts := newTestServer(t)
	ts.failNextChat.Store(http.StatusTooManyRequests)
	c := newTestClient(ts)
	if _, err := c.SpamProbability(context.Background(), "x"); err == nil {
		t.Fatal("chat 429 must surface as error")
	}
	// 429 не должен трактоваться как протухший токен: рефреша не было.
	if ts.oauthCalls.Load() != 1 || ts.chatCalls.Load() != 1 {
		t.Fatalf("oauth=%d chat=%d, want 1/1 (no refresh on non-401)",
			ts.oauthCalls.Load(), ts.chatCalls.Load())
	}
}

func TestParseProbability(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
		wantErr bool
	}{
		{"clean json", `{"spam_probability": 85}`, 85, false},
		{"json in code block", "```json\n{\"spam_probability\": 42}\n```", 42, false},
		{"json inside text", `Вот ответ: {"spam_probability": 7}.`, 7, false},
		{"json zero is a verdict", `{"spam_probability": 0}`, 0, false},
		{"json 0..1 scale", `{"spam_probability": 0.95}`, 95, false},
		// Ровно 1 — это 1% (промпт просит целые 0..100), НЕ шкала 0..1.
		{"one stays one percent", `{"spam_probability": 1}`, 1, false},
		{"leading-dot decimal", `.5`, 50, false},
		{"comma decimal", `0,95`, 95, false},
		{"bare number", `85`, 85, false},
		{"number in text", `Вероятность спама: 61%`, 61, false},
		{"clamp high", `{"spam_probability": 250}`, 100, false},
		{"clamp negative", `{"spam_probability": -5}`, 0, false},
		{"garbage", `не могу оценить`, 0, true},
		// Валидный JSON БЕЗ нужного ключа не должен тихо становиться нулём.
		{"json wrong key", `{"probability": 95}`, 95, false}, // спасает число-фолбек
		{"empty object", `{}`, 0, true},
		{"null", `null`, 0, true},
		// Несколько чисел — неоднозначно: fail-open честнее догадки.
		{"ambiguous prose", `По шкале от 0 до 100 я оцениваю это как 95`, 0, true},
		{"two numbers", `В чате 2 минуты, вероятность спама 5`, 0, true},
		// Единственное число, но не вероятность.
		{"single huge number", `1500`, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseProbability(tc.content)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v (p=%d)", err, tc.wantErr, p)
			}
			if !tc.wantErr && p != tc.want {
				t.Errorf("got %d, want %d", p, tc.want)
			}
		})
	}
}

func TestClampScale(t *testing.T) {
	tests := []struct {
		in   float64
		want int
	}{
		{0, 0},
		{0.95, 95}, // дробь 0..1 — шкала вероятности, переводим в проценты
		{1, 1},     // ровно 1 — это 1%, НЕ 100% (осознанное решение, см. clampScale)
		{1.5, 1},   // >1 не масштабируется, int() отбрасывает дробь
		{61, 61},
		{150, 100}, // клампы
		{-5, 0},
	}
	for _, tc := range tests {
		if got := clampScale(tc.in); got != tc.want {
			t.Errorf("clampScale(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestDisabledWithoutKey(t *testing.T) {
	if New("", "", "", "p").Enabled() {
		t.Fatal("empty auth key must mean disabled")
	}
	if !New("k", "", "", "p").Enabled() {
		t.Fatal("non-empty auth key must mean enabled")
	}
	var nilClient *Client
	if nilClient.Enabled() {
		t.Fatal("nil client must be disabled")
	}
}

func TestDefaults(t *testing.T) {
	c := New("k", "", "", "p")
	if c.scope != DefaultScope || c.model != DefaultModel {
		t.Fatalf("defaults not applied: scope=%q model=%q", c.scope, c.model)
	}
	if got := New("k", "S", "M", "p"); got.scope != "S" || got.model != "M" {
		t.Fatalf("overrides not applied: %q %q", got.scope, got.model)
	}
}

func TestEmbeddedCertParses(t *testing.T) {
	// Вшитый корень НУЦ Минцифры обязан быть валидным PEM-сертификатом —
	// AppendCertsFromPEM молча вернёт false для мусора (например, если файл
	// обновили DER-версией с госуслуг), и TLS к Сберу умрёт только в проде.
	block, _ := pem.Decode(trustedRootCA)
	if block == nil {
		t.Fatal("embedded CA is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("embedded CA does not parse: %v", err)
	}
	if !strings.Contains(cert.Subject.String(), "Russian Trusted Root CA") {
		t.Errorf("unexpected cert subject: %s", cert.Subject)
	}
	if cert.NotAfter.Before(time.Now().AddDate(0, 3, 0)) {
		t.Errorf("embedded CA expires soon (%s) — refresh it from gosuslugi.ru/crt", cert.NotAfter)
	}
	tr, ok := New("k", "", "", "p").http.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("client transport has no root CA pool configured")
	}
	if tr.Proxy == nil {
		t.Fatal("transport must keep DefaultTransport's Proxy (HTTPS_PROXY support)")
	}
}
