package bot

import (
	"testing"
	"time"
)

func TestGreetingInputConsumedOnce(t *testing.T) {
	b := &Bot{greetInput: map[int64]greetInputState{}}
	b.setGreetingInputPending(1, -100)

	chatID, ok, expired := b.takeGreetingInput(1)
	if !ok || expired || chatID != -100 {
		t.Fatalf("take = (%d, %v, %v), want (-100, true, false)", chatID, ok, expired)
	}
	if _, ok, expired := b.takeGreetingInput(1); ok || expired {
		t.Fatal("second take must find nothing (consumed)")
	}
	if _, ok, expired := b.takeGreetingInput(999); ok || expired {
		t.Fatal("unknown user must have no armed input")
	}
}

func TestGreetingInputExpires(t *testing.T) {
	b := &Bot{greetInput: map[int64]greetInputState{}}
	b.greetInput[2] = greetInputState{
		chatID:  -200,
		armedAt: time.Now().Add(-greetInputTTL - time.Minute),
	}
	// Просроченная запись не потребляется как валидная, но и не выглядит как
	// «ничего не было»: expired=true, чтобы админ получил ответ.
	if _, ok, expired := b.takeGreetingInput(2); ok || !expired {
		t.Fatalf("stale input: want (ok=false, expired=true), got (%v, %v)", ok, expired)
	}
	if _, exists := b.greetInput[2]; exists {
		t.Fatal("stale entry must be removed from the map")
	}
	// Свежая запись на границе TTL ещё живёт.
	b.greetInput[3] = greetInputState{
		chatID:  -300,
		armedAt: time.Now().Add(-greetInputTTL + time.Minute),
	}
	if chatID, ok, expired := b.takeGreetingInput(3); !ok || expired || chatID != -300 {
		t.Fatalf("fresh entry must survive: (%d, %v, %v)", chatID, ok, expired)
	}
}
