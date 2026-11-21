package bot

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

// healthbeat starts after polling and route registration. The marker belongs
// to this process even if Docker starts the same writable layer again.
func (b *Bot) healthbeat(ctx context.Context, handlerRunning func() bool) {
	path := os.Getenv("ANTISPAM_READY_FILE")
	if path == "" {
		return
	}
	for !handlerRunning() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		b.log.Warn("healthbeat boot ID", "err", err)
		return
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		b.log.Warn("healthbeat process stat", "err", err)
		return
	}
	start, err := processStart(string(stat))
	if err != nil {
		b.log.Warn("healthbeat process start", "err", err)
		return
	}
	marker := []byte(strings.TrimSpace(string(bootID)) + ":" + start)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for ctx.Err() == nil {
		err := os.WriteFile(path+".tmp", marker, 0o600) //nolint:gosec // the operator configures the readiness path
		if err == nil {
			err = os.Rename(path+".tmp", path) //nolint:gosec // the operator configures the readiness path
		}
		if err != nil {
			b.log.Warn("healthbeat write", "err", err, "path", path)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func processStart(stat string) (string, error) {
	// The filename in parentheses can itself contain spaces and parentheses.
	end := strings.LastIndex(stat, ") ")
	if end == -1 {
		return "", errors.New("missing process name in /proc stat")
	}
	values := strings.Fields(stat[end+2:])
	if len(values) < 20 {
		return "", errors.New("missing process start time in /proc stat")
	}
	return values[19], nil // Linux stat field 22 after removing fields 1 and 2.
}
