package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the real deploy script in a temporary checkout with local command fakes.
// Only host paths and PATH are redirected; no Docker daemon or network is used.
func TestAutoDeploy(t *testing.T) {
	for _, scenario := range []string{"success", "up-failed", "unhealthy", "notification-failed", "first-failed"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			write := func(path, text string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile("auto-deploy.sh")
			if err != nil {
				t.Fatal(err)
			}
			script := string(raw)
			script = strings.Replace(script, `export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"`, `export PATH="$FAKE_DIR/bin:/usr/bin:/bin"`, 1)
			for _, path := range []string{"/var/log/antispam-deploy.log", "/var/lib/antispam/bad-images", "/var/lib/antispam/notify-state", "/tmp/antispam-deploy.lock"} {
				script = strings.ReplaceAll(script, path, filepath.Join(dir, filepath.Base(path)))
			}
			write(filepath.Join(bin, "auto-deploy.sh"), script)
			write(filepath.Join(dir, ".env"), "BOT_TOKEN=not-a-real-token\nOWNER_IDS=1\n")
			write(filepath.Join(bin, "git"), "#!/bin/sh\n[ \"$1\" != rev-parse ] || echo same\nexit 0\n")
			write(filepath.Join(bin, "date"), "#!/bin/sh\necho 2026-11-06\n")
			for _, name := range []string{"flock", "sleep"} {
				write(filepath.Join(bin, name), "#!/bin/sh\nexit 0\n")
			}
			write(filepath.Join(bin, "docker"), `#!/bin/sh
echo "$*" >> "$FAKE_DIR/calls"
case "$1 $2" in
  'inspect --format')
    case "$3" in
      '{{.Image}}') [ "$SCENARIO" = first-failed ] || echo sha256:old;;
      '{{.State.Status}}') echo running;;
      '{{.RestartCount}}') echo 0;;
    esac;;
  'image inspect')
    if [ "$SCENARIO" = notification-failed ]; then echo sha256:old; else echo sha256:new; fi;;
  'compose pull') [ "$SCENARIO" != notification-failed ];;
  'compose up')
    if { [ "$SCENARIO" = up-failed ] || [ "$SCENARIO" = first-failed ]; } && [ ! -e "$FAKE_DIR/failed" ]; then
      touch "$FAKE_DIR/failed"
      exit 1
    fi;;
  'exec menand-antispam') [ "$SCENARIO" != unhealthy ];;
esac
`)
			write(filepath.Join(bin, "curl"), `#!/bin/sh
request=$(cat)
case "$request" in
  *getMe*) echo '{"ok":true}';;
  *)
    if [ "$SCENARIO" = notification-failed ]; then
      case "$*" in *-f*) exit 22;; esac
    fi;;
esac
`)
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(bin, "auto-deploy.sh"))
			cmd.Env = []string{"PATH=/usr/bin:/bin", "FAKE_DIR=" + dir, "SCENARIO=" + scenario}
			out, runErr := cmd.CombinedOutput()
			callsRaw, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatalf("no Docker calls: %v\n%s", err, out)
			}
			calls := string(callsRaw)
			if strings.Contains(calls, "image prune") {
				t.Fatal("deploy must not prune other projects' images")
			}
			switch scenario {
			case "success":
				if runErr != nil || !strings.Contains(calls, "image rm sha256:old") {
					t.Fatalf("successful deploy must clean only old bot image: %v\n%s\n%s", runErr, out, calls)
				}
			case "up-failed", "unhealthy":
				if runErr == nil || !strings.Contains(calls, "tag sha256:old ghcr.io/menand/antispambot:latest") || strings.Count(calls, "compose up") != 2 {
					t.Fatalf("failed deploy must attempt rollback: %v\n%s\n%s", runErr, out, calls)
				}
				if strings.Contains(calls, "image rm") {
					t.Fatal("failed deploy must keep the rollback image")
				}
			case "notification-failed":
				if _, err := os.Stat(filepath.Join(dir, "notify-state")); !os.IsNotExist(err) {
					t.Fatal("HTTP error must not mark the alert delivered")
				}
			case "first-failed":
				if runErr == nil || strings.Contains(calls, "tag ") || !strings.Contains(string(out), "no previous image") {
					t.Fatalf("first deploy failure must report missing rollback image: %v\n%s\n%s", runErr, out, calls)
				}
			}
		})
	}
}
