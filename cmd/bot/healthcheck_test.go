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
		marker  string
		healthy bool
	}{
		{name: "missing"},
		{name: "current instance", age: time.Minute, marker: "boot-id:900", healthy: true},
		{name: "stale", age: 10 * time.Minute, marker: "boot-id:900"},
		{name: "previous instance", age: time.Second, marker: "boot-id:800"},
		{name: "previous boot", age: time.Second, marker: "old-boot:900"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Supply a deterministic PID 1 identity on Linux and macOS while
			// still exercising the real healthcheck and marker timestamps.
			cat := `#!/bin/sh
case "$1" in
/proc/1/stat) echo '1 (bot ) test) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 900 0';;
/proc/sys/kernel/random/boot_id) echo boot-id;;
*) exec /bin/cat "$@";;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "cat"), []byte(cat), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "ready")
			if tc.marker != "" {
				if err := os.WriteFile(path, []byte(tc.marker), 0o600); err != nil {
					t.Fatal(err)
				}
				mtime := time.Now().Add(-tc.age)
				if err := os.Chtimes(path, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "sh", "../../scripts/healthcheck.sh")
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "ANTISPAM_READY_FILE=" + path}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.healthy {
				t.Fatalf("healthy=%v, want %v: %v %s", err == nil, tc.healthy, err, out)
			}
		})
	}
}
