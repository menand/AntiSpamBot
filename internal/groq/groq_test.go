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

func TestIsSpamOK(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"SPAM"}}]}`))
	})
	spam, err := c.IsSpam(context.Background(), "текст")
	if err != nil || !spam {
		t.Fatalf("want spam=true, got %v err=%v", spam, err)
	}
}

func TestIsSpamGarbageVerdict(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"не могу оценить"}}]}`))
	})
	if _, err := c.IsSpam(context.Background(), "x"); err == nil {
		t.Fatal("want parse error (fail-open at caller), got nil")
	}
}

func TestIsSpam429(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if _, err := c.IsSpam(context.Background(), "x"); err == nil {
		t.Fatal("want error on 429, got nil")
	}
}

func TestIsSpamTimeout(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.IsSpam(ctx, "x"); err == nil {
		t.Fatal("want timeout error, got nil")
	}
}

func TestClassifyCustomSystem(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	})
	spam, err := c.Classify(context.Background(), "МОЙ ПРОМПТ", "факты")
	if err != nil || spam {
		t.Fatalf("want spam=false, got %v err=%v", spam, err)
	}
	if gotSystem != "МОЙ ПРОМПТ" {
		t.Fatalf("system prompt not passed through: %q", gotSystem)
	}
}

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
		wantErr bool
	}{
		{"bare spam", `SPAM`, true, false},
		{"bare ok", `OK`, false, false},
		{"lowercase", `spam`, true, false},
		{"russian spam", `СПАМ`, true, false},
		{"russian ok", `ок`, false, false},
		{"trailing dot", `SPAM.`, true, false},
		{"quoted", `"OK"`, false, false},
		{"whitespace", "\n OK \n", false, false},
		{"framed spam", `Verdict: SPAM`, true, false},
		{"framed ok", `Ответ: OK.`, false, false},
		// Отрицание перед словом — это OK, а не SPAM: инверсия означала бы
		// ложный глобальный бан.
		{"negated russian", `Не спам`, false, false},
		{"negated english", `NOT SPAM`, false, false},
		{"negated sentence", `Это не спам.`, false, false},
		{"ok with negated spam", `OK, это не SPAM`, false, false},
		// Неоднозначность — ошибка, а не догадка (fail-open у вызывающего).
		{"both words", `SPAM или OK — решай сам`, false, true},
		{"garbage", `не могу оценить`, false, true},
		{"empty", ``, false, true},
		{"old json format", `{"spam_probability": 93}`, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spam, err := ParseVerdict(tc.content)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v (spam=%v)", err, tc.wantErr, spam)
			}
			if !tc.wantErr && spam != tc.want {
				t.Errorf("got %v, want %v", spam, tc.want)
			}
		})
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
