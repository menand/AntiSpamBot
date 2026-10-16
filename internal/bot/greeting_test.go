package bot

import (
	"testing"
	"time"
)

func TestGreetingInputConsumedOnce(t *testing.T) {
	b := &Bot{greetInput: map[int64]greetInputState{}}
	b.setGreetingInputPending(1, -100, welcomeReply)

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
