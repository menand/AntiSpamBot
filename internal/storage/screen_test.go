package storage

import (
	"context"
	"testing"
	"time"
)

func TestSwapDMScreenRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()

	if old, err := db.SwapDMScreen(ctx, 7, "panel", 100, now); err != nil || old != 0 {
		t.Fatalf("first swap: old=%d err=%v, want 0", old, err)
	}
	if old, err := db.SwapDMScreen(ctx, 7, "panel", 101, now); err != nil || old != 100 {
		t.Fatalf("second swap: old=%d err=%v, want 100", old, err)
	}
	// Виды экранов независимы: чужой kind не видит предыдущий «panel».
	if old, err := db.SwapDMScreen(ctx, 7, "logs", 500, now); err != nil || old != 0 {
		t.Fatalf("other kind: old=%d err=%v, want 0", old, err)
	}
	if old, _ := db.SwapDMScreen(ctx, 7, "panel", 102, now); old != 101 {
		t.Fatalf("panel after logs: old=%d, want 101", old)
	}
	// Юзеры независимы.
	if old, _ := db.SwapDMScreen(ctx, 8, "panel", 900, now); old != 0 {
		t.Fatalf("other user: old=%d, want 0", old)
	}
	// Тот же id (повторный своп того же сообщения) — удалять нечего.
	if old, _ := db.SwapDMScreen(ctx, 7, "panel", 102, now); old != 0 {
		t.Fatalf("same id swap: old=%d, want 0", old)
	}
}
