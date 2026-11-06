package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestHealthcheckHeartbeatFreshness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		missing bool
		healthy bool
	}{
		{name: "missing", missing: true},
		{name: "fresh", age: time.Minute, healthy: true},
		{name: "stale", age: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if !tc.missing {
				p := filepath.Join(dir, ".heartbeat")
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				mtime := time.Now().Add(-tc.age)
				if err := os.Chtimes(p, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "sh", "../../scripts/healthcheck.sh")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DB_PATH=" + filepath.Join(dir, "bot.db")}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.healthy {
				t.Fatalf("healthy=%v, want %v: %v %s", err == nil, tc.healthy, err, out)
			}
		})
	}
}
