package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego/telegoapi"
)

func TestRetryAfterDelay(t *testing.T) {
	if _, ok := retryAfterDelay(nil); ok {
		t.Fatal("nil error must not yield a delay")
	}
	if _, ok := retryAfterDelay(errors.New("dial tcp: timeout")); ok {
		t.Fatal("non-API error must not yield a delay")
	}
	if _, ok := retryAfterDelay(&telegoapi.Error{ErrorCode: 429}); ok {
		t.Fatal("429 without parameters must not yield a delay")
	}
	if _, ok := retryAfterDelay(&telegoapi.Error{
		ErrorCode: 400, Parameters: &telegoapi.ResponseParameters{RetryAfter: 5},
	}); ok {
		t.Fatal("non-429 must not yield a delay")
	}

	flood := &telegoapi.Error{ErrorCode: 429, Parameters: &telegoapi.ResponseParameters{RetryAfter: 7}}
	d, ok := retryAfterDelay(flood)
	if !ok || d != 7*time.Second {
		t.Fatalf("got (%v, %v), want (7s, true)", d, ok)
	}
	// Обёрнутая ошибка (как из fmt.Errorf на call-site) тоже распознаётся.
	d, ok = retryAfterDelay(fmt.Errorf("restrict: %w", flood))
	if !ok || d != 7*time.Second {
		t.Fatalf("wrapped: got (%v, %v), want (7s, true)", d, ok)
	}
}

func TestRetryWithSucceedsAfterFailures(t *testing.T) {
	calls := 0
	err := retryWith(context.Background(), []time.Duration{0, 0, 0}, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d, want nil after 3rd call", err, calls)
	}
}

func TestRetryWithReturnsLastError(t *testing.T) {
	last := errors.New("still down")
	calls := 0
	err := retryWith(context.Background(), []time.Duration{0, 0}, func() error {
		calls++
		return last
	})
	if !errors.Is(err, last) || calls != 2 {
		t.Fatalf("err=%v calls=%d, want last error after 2 attempts", err, calls)
	}
}

func TestRetryWithHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	err := retryWith(ctx, []time.Duration{0, time.Hour}, func() error {
		calls++
		return errors.New("transient")
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want ctx deadline error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("second attempt must not run after ctx cancel, calls=%d", calls)
	}
}

func TestRetryWithStretchesToRetryAfter(t *testing.T) {
	// Бэкоффы {0, 0} + flood с retry_after=1s: без стретча вторая попытка ушла
	// бы мгновенно (calls==2, nil); со стретчем 1-секундная пауза упирается в
	// 100ms-дедлайн ctx — calls==1 доказывает растяжку без секундного сна.
	flood := &telegoapi.Error{ErrorCode: 429, Parameters: &telegoapi.ResponseParameters{RetryAfter: 1}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	calls := 0
	err := retryWith(ctx, []time.Duration{0, 0}, func() error {
		calls++
		return flood
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error from the stretched wait, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("stretch must delay the 2nd attempt past the deadline, calls=%d", calls)
	}
	// Исходная API-ошибка не должна теряться за ctx-ошибкой.
	if !strings.Contains(err.Error(), "last attempt error") {
		t.Fatalf("ctx error must carry the underlying API error, got: %v", err)
	}
}
