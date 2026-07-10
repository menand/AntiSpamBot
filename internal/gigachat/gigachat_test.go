package gigachat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync/atomic"
	"testing"
	"time"
)

var rqUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// testServer поднимает мок обоих эндпоинтов. oauthCalls/chatCalls — счётчики
// для проверки кэширования токена; chatStatus управляет ответом chat.
type testServer struct {
	*httptest.Server
	oauthCalls  atomic.Int32
	chatCalls   atomic.Int32
	tokenTTL    time.Duration
	chatContent string
	// перед каждым chat-ответом снимается верхний статус из очереди; пустая
	// очередь = 200.
	chatStatuses []int
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
		if len(ts.chatStatuses) > 0 {
			status := ts.chatStatuses[0]
			ts.chatStatuses = ts.chatStatuses[1:]
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
		}
		if got := r.Header.Get("Authorization"); got == "Bearer " || got == "" {
			t.Errorf("chat has no bearer token: %q", got)
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
	ts.chatStatuses = []int{http.StatusUnauthorized} // первый chat — 401
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
	ts.chatStatuses = []int{http.StatusTooManyRequests}
	c := newTestClient(ts)
	if _, err := c.SpamProbability(context.Background(), "x"); err == nil {
		t.Fatal("chat 429 must surface as error")
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
		{"bare number", `85`, 85, false},
		{"number in text", `Вероятность спама: 61%`, 61, false},
		{"clamp high", `{"spam_probability": 250}`, 100, false},
		{"clamp negative", `{"spam_probability": -5}`, 0, false},
		{"garbage", `не могу оценить`, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseProbability(tc.content)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && p != tc.want {
				t.Errorf("got %d, want %d", p, tc.want)
			}
		})
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

func TestEmbeddedCertLoads(t *testing.T) {
	// Вшитый корень НУЦ Минцифры обязан парситься — иначе TLS к Сберу мёртв.
	c := New("k", "", "", "p")
	if c.http == nil || c.http.Transport == nil {
		t.Fatal("client transport not configured")
	}
	if len(trustedRootCA) < 1000 {
		t.Fatalf("embedded CA suspiciously small: %d bytes", len(trustedRootCA))
	}
}
