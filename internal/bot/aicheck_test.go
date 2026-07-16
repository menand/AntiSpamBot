package bot

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFormatProviderCheck(t *testing.T) {
	ok := formatProviderCheck("groq", "llama-3.1-8b-instant", false, 700*time.Millisecond, nil)
	for _, want := range []string{"✅ groq", "llama-3.1-8b-instant", "0.7 с", "вердикт OK", "коннект есть"} {
		if !strings.Contains(ok, want) {
			t.Errorf("success line missing %q: %s", want, ok)
		}
	}
	if spam := formatProviderCheck("groq", "m", true, time.Second, nil); !strings.Contains(spam, "вердикт SPAM") {
		t.Errorf("spam verdict line missing: %s", spam)
	}

	fail := formatProviderCheck("gigachat", "GigaChat", false, 2*time.Second,
		errors.New(`gigachat status 401: {"message":"<b>Unauthorized</b>"}`))
	if !strings.Contains(fail, "❌ gigachat") {
		t.Errorf("failure line must start with cross: %s", fail)
	}
	// HTML из тела ошибки обязан быть экранирован — сообщение шлётся ModeHTML.
	if strings.Contains(fail, "<b>") || !strings.Contains(fail, "&lt;b&gt;") {
		t.Errorf("error text must be HTML-escaped: %s", fail)
	}

	// Длиннющая ошибка режется, чтобы влезть в сообщение.
	long := formatProviderCheck("groq", "m", false, time.Second, errors.New(strings.Repeat("х", 500)))
	if got := len([]rune(long)); got > 300 {
		t.Errorf("long error must be truncated, line is %d runes", got)
	}
	if !strings.Contains(long, "…") {
		t.Errorf("truncated error must end with ellipsis: %s", long)
	}
}
