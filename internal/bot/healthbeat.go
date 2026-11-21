package bot

import (
	"errors"
	"os"
	"strings"
	"time"
)

// pollHeartbeat refreshes readiness after a successful getUpdates call.
// The marker belongs to this process even when Docker reuses its writable layer.
func (b *Bot) pollHeartbeat() func() {
	path := os.Getenv("ANTISPAM_READY_FILE")
	if path == "" {
		return func() {}
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		b.log.Warn("healthbeat boot ID", "err", err)
		return func() {}
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		b.log.Warn("healthbeat process stat", "err", err)
		return func() {}
	}
	start, err := processStart(string(stat))
	if err != nil {
		b.log.Warn("healthbeat process start", "err", err)
		return func() {}
	}
	marker := []byte(strings.TrimSpace(string(bootID)) + ":" + start)
	var lastWrite time.Time
	return func() {
		if time.Since(lastWrite) < time.Minute {
			return
		}
		err := os.WriteFile(path+".tmp", marker, 0o600) //nolint:gosec // the operator configures the readiness path
		if err == nil {
			err = os.Rename(path+".tmp", path) //nolint:gosec // the operator configures the readiness path
		}
		if err != nil {
			b.log.Warn("healthbeat write", "err", err, "path", path)
			return
		}
		lastWrite = time.Now()
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
