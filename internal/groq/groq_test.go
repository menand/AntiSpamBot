package groq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New("test-key", "")
	c.endpoint = srv.URL
	return c
}

func TestSpamProbabilityOK(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"spam_probability\": 93}"}}]}`))
	})
	p, err := c.SpamProbability(context.Background(), "текст")
	if err != nil || p != 93 {
		t.Fatalf("want 93, got %d err=%v", p, err)
	}
}

func TestSpamProbabilityClamp(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"spam_probability\": 250}"}}]}`))
	})
	p, err := c.SpamProbability(context.Background(), "x")
	if err != nil || p != 100 {
		t.Fatalf("want clamp to 100, got %d err=%v", p, err)
	}
}

func TestSpamProbabilityBadJSON(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"это не json"}}]}`))
	})
	if _, err := c.SpamProbability(context.Background(), "x"); err == nil {
		t.Fatal("want parse error (fail-open at caller), got nil")
	}
}

func TestSpamProbability429(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if _, err := c.SpamProbability(context.Background(), "x"); err == nil {
		t.Fatal("want error on 429, got nil")
	}
}

func TestSpamProbabilityTimeout(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.SpamProbability(ctx, "x"); err == nil {
		t.Fatal("want timeout error, got nil")
	}
}

func TestProbabilityCustomSystem(t *testing.T) {
	// Кастомный системный промпт (профиль-чек) должен дойти до API как есть.
	var gotSystem string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			gotSystem = req.Messages[0].Content
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"spam_probability\": 42}"}}]}`))
	})
	p, err := c.Probability(context.Background(), "МОЙ ПРОМПТ", "факты")
	if err != nil || p != 42 {
		t.Fatalf("want 42, got %d err=%v", p, err)
	}
	if gotSystem != "МОЙ ПРОМПТ" {
		t.Fatalf("system prompt not passed through: %q", gotSystem)
	}
}

func TestDisabledWithoutKey(t *testing.T) {
	if New("", "").Enabled() {
		t.Fatal("empty key must mean disabled")
	}
	if !New("k", "").Enabled() {
		t.Fatal("non-empty key must mean enabled")
	}
}
