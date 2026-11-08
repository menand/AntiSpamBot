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
	for _, scenario := range []string{
		"success", "tag-conflict", "unchanged", "containerd-unchanged", "containerd-success",
		"up-noop", "containerd-up-noop", "up-failed", "unhealthy", "early-restart",
		"rollback-unhealthy", "rollback-wrong-image", "rollback-up-failed",
		"fetch-failed", "pull-failed", "getme-failed", "notification-failed", "first-failed",
	} {
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
			helper, err := os.ReadFile("deploy-notify.sh")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o700); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(dir, "scripts", "deploy-notify.sh"), string(helper))
			write(filepath.Join(dir, ".env"), "BOT_TOKEN=not-a-real-token\nOWNER_IDS=1\n")
			write(filepath.Join(bin, "git"), `#!/bin/sh
if [ "$1" = fetch ]; then
  if [ "$SCENARIO" = fetch-failed ]; then exit 1; fi
  if [ "$SCENARIO" = tag-conflict ]; then
    for arg in "$@"; do [ "$arg" != --tags ] || exit 1; done
  fi
fi
[ "$1" != rev-parse ] || echo same
exit 0
`)
			write(filepath.Join(bin, "date"), "#!/bin/sh\necho 2026-11-06\n")
			for _, name := range []string{"flock", "sleep"} {
				write(filepath.Join(bin, name), "#!/bin/sh\nexit 0\n")
			}
			write(filepath.Join(bin, "docker"), `#!/bin/sh
echo "$*" >> "$FAKE_DIR/calls"
case "$1 $2" in
  'inspect --format')
    case "$3" in
      '{{.Image}}')
        if [ "$SCENARIO" != first-failed ]; then
          active=old
          if [ -e "$FAKE_DIR/deployed" ]; then active=new; fi
          if [ -e "$FAKE_DIR/rollback" ] && [ "$SCENARIO" != rollback-wrong-image ]; then active=old; fi
          case "$SCENARIO" in
            containerd-*)
              if [ "$SCENARIO" = containerd-unchanged ]; then active=new; fi
              echo "sha256:config-$active";;
            *) echo "sha256:$active";;
          esac
        fi;;
      *ImageManifestDescriptor*platform*)
        case "$SCENARIO" in containerd-*) echo linux/amd64;; esac;;
      *ImageManifestDescriptor*)
        case "$SCENARIO" in
          containerd-*)
            if [ "$SCENARIO" = containerd-unchanged ] || [ -e "$FAKE_DIR/deployed" ]; then echo sha256:manifest-new; else echo sha256:manifest-old; fi;;
        esac;;
      '{{.State.Status}}') echo running;;
      '{{.RestartCount}}')
        if [ "$SCENARIO" = early-restart ] && [ ! -e "$FAKE_DIR/rollback" ]; then echo 1; else echo 0; fi;;
    esac;;
  'image inspect')
    case "$4" in *Config.Labels*) exit 0;; esac
    case "$SCENARIO" in
      notification-failed|unchanged) echo sha256:old;;
      containerd-*)
        if [ "$3" = --platform ]; then echo sha256:manifest-new; else echo sha256:index-new; fi;;
      *) echo sha256:new;;
    esac;;
  'compose pull')
    case "$SCENARIO" in notification-failed|pull-failed) exit 1;; esac;;
  'compose up')
    if [ "$SCENARIO" = rollback-up-failed ] && [ -e "$FAKE_DIR/rollback" ]; then exit 1; fi
    if { [ "$SCENARIO" = up-failed ] || [ "$SCENARIO" = first-failed ]; } && [ ! -e "$FAKE_DIR/failed" ]; then
      touch "$FAKE_DIR/failed"
      exit 1
    fi
    case "$SCENARIO" in up-noop|containerd-up-noop) ;; *) touch "$FAKE_DIR/deployed";; esac;;
  'tag sha256:old'|'tag sha256:config-old') touch "$FAKE_DIR/rollback";;
  'exec menand-antispam')
    if [ -e "$FAKE_DIR/rollback" ]; then
      [ "$SCENARIO" != rollback-unhealthy ]
    else
      case "$SCENARIO" in unhealthy|rollback-unhealthy|rollback-wrong-image|rollback-up-failed) exit 1;; esac
    fi;;
esac
`)
			write(filepath.Join(bin, "curl"), `#!/bin/sh
request=$(cat)
echo "$*" >> "$FAKE_DIR/notifications"
case "$request" in
  *getMe*) [ "$SCENARIO" != getme-failed ] || exit 22; echo '{"ok":true}';;
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
			if err != nil && !(scenario == "fetch-failed" && os.IsNotExist(err)) {
				t.Fatalf("no Docker calls: %v\n%s", err, out)
			}
			calls := string(callsRaw)
			if strings.Contains(calls, "image prune") {
				t.Fatal("deploy must not prune other projects' images")
			}
			switch scenario {
			case "unchanged", "containerd-unchanged":
				if runErr != nil || strings.Contains(calls, "compose up") || strings.Contains(calls, "image rm") || len(out) != 0 {
					t.Fatalf("unchanged image must leave the container alone and stay silent: %v\n%s\n%s", runErr, out, calls)
				}
				if _, err := os.Stat(filepath.Join(dir, "notifications")); !os.IsNotExist(err) {
					t.Fatal("unchanged image must not call Telegram")
				}
			case "success", "tag-conflict":
				if runErr != nil || !strings.Contains(calls, "image rm sha256:old") {
					t.Fatalf("successful deploy must clean only old bot image: %v\n%s\n%s", runErr, out, calls)
				}
			case "containerd-success":
				if runErr != nil || strings.Count(calls, "compose up") != 1 || !strings.Contains(calls, "image rm sha256:config-old") {
					t.Fatalf("new platform manifest must deploy successfully: %v\n%s\n%s", runErr, out, calls)
				}
				if !strings.Contains(calls, "image inspect --platform linux/amd64 --format {{.Id}} sha256:index-new") {
					t.Fatal("manifest comparison must use the pulled immutable image and the container's platform")
				}
			case "fetch-failed", "pull-failed":
				if runErr == nil || strings.Contains(calls, "compose up") || strings.Contains(calls, "image inspect") {
					t.Fatalf("fetch/pull failure must abort before deploying a cached image: %v\n%s\n%s", runErr, out, calls)
				}
				notifications, err := os.ReadFile(filepath.Join(dir, "notifications"))
				if err != nil || !strings.Contains(string(notifications), "Повторная попытка — автоматически через минуту") {
					t.Fatalf("postponed update must explain automatic retry: %v\n%s", err, notifications)
				}
			case "getme-failed":
				notifications, err := os.ReadFile(filepath.Join(dir, "notifications"))
				if runErr != nil || err != nil || strings.Contains(calls, "tag ") || strings.Contains(string(notifications), "✅") || !strings.Contains(string(notifications), "Telegram API недоступен") {
					t.Fatalf("Telegram outage must warn while keeping the healthy image: %v / %v\n%s\n%s", runErr, err, out, notifications)
				}
			case "up-failed", "unhealthy", "early-restart", "up-noop", "containerd-up-noop", "rollback-unhealthy", "rollback-wrong-image", "rollback-up-failed":
				previous := "sha256:old"
				if scenario == "containerd-up-noop" {
					previous = "sha256:config-old"
				}
				if runErr == nil || !strings.Contains(calls, "tag "+previous+" ghcr.io/menand/antispambot:latest") || strings.Count(calls, "compose up") != 2 {
					t.Fatalf("failed deploy must attempt rollback: %v\n%s\n%s", runErr, out, calls)
				}
				if strings.Contains(calls, "image rm") {
					t.Fatal("failed deploy must keep the rollback image")
				}
				if strings.Contains(scenario, "noop") && !strings.Contains(string(out), "запущен другой образ") {
					t.Fatalf("successful compose without an image change must fail smoke: %s", out)
				}
				notifications, err := os.ReadFile(filepath.Join(dir, "notifications"))
				if err != nil || strings.Contains(string(notifications), "✅") {
					t.Fatalf("failed deploy must not send a success notification: %v\n%s", err, notifications)
				}
				if strings.HasPrefix(scenario, "rollback-") {
					if !strings.Contains(string(out), "ROLLBACK FAILED") || !strings.Contains(string(notifications), "НЕ подтверждён") {
						t.Fatalf("broken rollback must report unconfirmed recovery: %s\n%s", out, notifications)
					}
				} else if !strings.Contains(string(out), "rollback done") || !strings.Contains(string(notifications), "прежний образ работает") {
					t.Fatalf("rollback must verify the previous image before reporting recovery: %s\n%s", out, notifications)
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
