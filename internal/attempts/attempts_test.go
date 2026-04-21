package attempts

import (
	"testing"
	"time"
)

func TestIncrementCountsPerKey(t *testing.T) {
	tr := NewTracker(time.Minute)
	if got := tr.Increment(1, 10); got != 1 {
		t.Fatalf("first increment: got %d, want 1", got)
	}
	if got := tr.Increment(1, 10); got != 2 {
		t.Fatalf("second increment: got %d, want 2", got)
	}
	if got := tr.Increment(1, 11); got != 1 {
		t.Fatalf("different user: got %d, want 1", got)
	}
	if got := tr.Increment(2, 10); got != 1 {
		t.Fatalf("different chat: got %d, want 1", got)
	}
}

func TestResetClearsCount(t *testing.T) {
	tr := NewTracker(time.Minute)
	tr.Increment(1, 10)
	tr.Increment(1, 10)
	tr.Reset(1, 10)
	if got := tr.Increment(1, 10); got != 1 {
		t.Fatalf("after Reset: got %d, want 1", got)
	}
}

func TestIncrementResetsAfterTTL(t *testing.T) {
	tr := NewTracker(10 * time.Millisecond)
	tr.Increment(1, 10)
	tr.Increment(1, 10)
	time.Sleep(20 * time.Millisecond)
	if got := tr.Increment(1, 10); got != 1 {
		t.Fatalf("after TTL expiry: got %d, want 1", got)
	}
}
