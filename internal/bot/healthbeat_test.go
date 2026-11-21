package bot

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPollHeartbeatWritesOnSuccess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the Linux process identity from /proc")
	}
	b, _, _ := newFlowBot(t)
	path := filepath.Join(t.TempDir(), "heartbeat")
	t.Setenv("ANTISPAM_READY_FILE", path)
	heartbeat := b.pollHeartbeat()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("heartbeat appeared before a successful poll: %v", err)
	}
	heartbeat()
	if value, err := os.ReadFile(path); err != nil || len(value) == 0 {
		t.Fatalf("missing heartbeat after success: %q / %v", value, err)
	}
}

func TestProcessStartIdentity(t *testing.T) {
	const stat = "1 (bot ) test) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 900 0"
	if start, err := processStart(stat); err != nil || start != "900" {
		t.Fatalf("got start=%q, err=%v", start, err)
	}
}
