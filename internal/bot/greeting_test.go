package bot

import (
	"testing"
	"time"
)

func TestGreetingInputConsumedOnce(t *testing.T) {
	b := &Bot{greetInput: map[int64]greetInputState{}}
	b.setGreetingInputPending(1, -100, welcomeReply, 0)

	chatID, kind, ok, expired := b.takeGreetingInput(1)
	if !ok || expired || chatID != -100 || kind != welcomeReply {
		t.Fatalf("take = (%d, %q, %v, %v), want (-100, reply, true, false)", chatID, kind, ok, expired)
	}
	if _, _, ok, expired := b.takeGreetingInput(1); ok || expired {
		t.Fatal("second take must find nothing (consumed)")
	}
	if _, _, ok, expired := b.takeGreetingInput(999); ok || expired {
		t.Fatal("unknown user must have no armed input")
	}
}

// TestGreetingInputPromptMatch — мост DM→чат отличает ответ на промпт флоу от
// ответа на уведомление без связки: свежий промпт пропускает текст в флоу,
// протухший — тоже (флоу сам честно скажет «запрос устарел»), а чужой/нулевой
// id и неизвестный юзер не совпадают вовсе.
func TestGreetingInputPromptMatch(t *testing.T) {
	b := &Bot{greetInput: map[int64]greetInputState{}}
	b.setGreetingInputPending(1, -100, welcomeGreet, 555)

	if fresh, expired := b.greetingInputPromptMatch(1, 555); !fresh || expired {
		t.Fatalf("fresh prompt: got fresh=%v expired=%v", fresh, expired)
	}
	for _, msgID := range []int{556, 0, -1} {
		if fresh, expired := b.greetingInputPromptMatch(1, msgID); fresh || expired {
			t.Fatalf("msg %d: got fresh=%v expired=%v, want false/false", msgID, fresh, expired)
		}
	}
	if fresh, expired := b.greetingInputPromptMatch(999, 555); fresh || expired {
		t.Fatalf("unknown user: got fresh=%v expired=%v, want false/false", fresh, expired)
	}

	// Протухший арм на тот же промпт — expired, но не «связка устарела».
	b.greetInput[2] = greetInputState{
		chatID:      -200,
		kind:        welcomeGreet,
		promptMsgID: 777,
		armedAt:     time.Now().Add(-greetInputTTL - time.Minute),
	}
	if fresh, expired := b.greetingInputPromptMatch(2, 777); fresh || !expired {
		t.Fatalf("stale prompt: got fresh=%v expired=%v, want false/true", fresh, expired)
	}
	// Проверка не потребляет арм: его забирает takeGreetingInput.
	if _, ok := b.greetInput[2]; !ok {
		t.Fatal("prompt match must not consume the armed input")
	}
}

func TestGreetingInputExpires(t *testing.T) {
	b := &Bot{greetInput: map[int64]greetInputState{}}
	b.greetInput[2] = greetInputState{
		chatID:  -200,
		kind:    welcomeGreet,
		armedAt: time.Now().Add(-greetInputTTL - time.Minute),
	}
	// Просроченная запись не потребляется как валидная, но и не выглядит как
	// «ничего не было»: expired=true, чтобы админ получил ответ. Род текста
	// переживает истечение — отказ называет, ЧТО не сохранилось.
	if _, kind, ok, expired := b.takeGreetingInput(2); ok || !expired || kind != welcomeGreet {
		t.Fatalf("stale input: want (ok=false, expired=true, kind=greet), got (%v, %v, %q)", ok, expired, kind)
	}
	if _, exists := b.greetInput[2]; exists {
		t.Fatal("stale entry must be removed from the map")
	}
	// Свежая запись на границе TTL ещё живёт.
	b.greetInput[3] = greetInputState{
		chatID:  -300,
		kind:    welcomeGreet,
		armedAt: time.Now().Add(-greetInputTTL + time.Minute),
	}
	if chatID, kind, ok, expired := b.takeGreetingInput(3); !ok || expired || chatID != -300 || kind != welcomeGreet {
		t.Fatalf("fresh entry must survive: (%d, %q, %v, %v)", chatID, kind, ok, expired)
	}
}
