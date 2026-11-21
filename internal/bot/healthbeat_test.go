package bot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthbeatWaitsForHandler(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the Linux process identity from /proc")
	}
	b, _, _ := newFlowBot(t)
	path := filepath.Join(t.TempDir(), "heartbeat")
	t.Setenv("ANTISPAM_READY_FILE", path)
	ctx, cancel := context.WithCancel(t.Context())
	firstCheck := make(chan struct{})
	var running atomic.Bool
	var observed sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.healthbeat(ctx, func() bool {
			ready := running.Load()
			observed.Do(func() { close(firstCheck) })
			return ready
		})
	}()
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-firstCheck:
	case <-time.After(2 * time.Second):
		t.Fatal("healthbeat did not check the handler")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("heartbeat appeared while handler was uninitialized: %v", err)
	}
	running.Store(true)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		marker, err := os.ReadFile(path)
		if err == nil && len(marker) > 0 {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("running handler did not publish a heartbeat: %v", err)
		}
	}
}

func TestProcessStartIdentity(t *testing.T) {
	const stat = "1 (bot ) test) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 900 0"
	if start, err := processStart(stat); err != nil || start != "900" {
		t.Fatalf("got start=%q, err=%v", start, err)
	}
}
