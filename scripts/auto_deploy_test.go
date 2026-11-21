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
		"up-noop", "containerd-up-noop", "up-failed", "unhealthy", "previous-heartbeat", "early-restart",
		"rollback-unhealthy", "rollback-wrong-image", "rollback-up-failed",
		"fetch-failed", "pull-failed", "getme-failed", "notification-failed", "first-failed", "first-success",
		"first-interrupted", "first-interrupted-before-inspect", "first-interrupted-new-tag", "first-interrupted-unhealthy", "first-interrupted-wrong-volume", "first-interrupted-replaced",
		"retention",
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
			for _, path := range []string{"/var/log/antispam-deploy.log", "/var/lib/antispam/bad-images", "/var/lib/antispam/notify-state", "/var/lib/antispam/releases", "/tmp/antispam-deploy.lock"} {
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
			worker, err := os.ReadFile("deploy-release.sh")
			if err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(dir, "worker"), "echo admitted >> \"$FAKE_DIR/controllers\"\n"+string(worker))
			write(filepath.Join(bin, "git"), `#!/bin/sh
old=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
new=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
next=cccccccccccccccccccccccccccccccccccccccc
broken=dddddddddddddddddddddddddddddddddddddddd
echo "$*" >> "$FAKE_DIR/git-calls"
case "$1" in
  fetch)
    [ "$SCENARIO" != fetch-failed ] || exit 1
    for arg in "$@"; do [ "$arg" != --tags ] || exit 1; done;;
  merge-base) [ "$3" = "$old" ] || [ "$3" = "$new" ] || [ "$3" = "$next" ] || [ "$3" = "$broken" ];;
  show)
    case "$2" in
      "$old:docker-compose.yml") echo 'previous compose';;
      "$new:docker-compose.yml") echo 'candidate compose';;
      "$next:docker-compose.yml") echo 'next compose';;
      "$broken:docker-compose.yml") echo 'broken compose';;
      "$new:scripts/deploy-release.sh"|"$next:scripts/deploy-release.sh"|"$broken:scripts/deploy-release.sh") cat "$FAKE_DIR/worker";;
      "$new:scripts/deploy-notify.sh"|"$next:scripts/deploy-notify.sh"|"$broken:scripts/deploy-notify.sh") cat "$FAKE_DIR/scripts/deploy-notify.sh";;
      *) exit 1;;
    esac;;
  *) exit 1;;
esac
`)
			write(filepath.Join(bin, "date"), "#!/bin/sh\nif [ \"$1\" = -d ]; then echo 2000000000; else echo 2026-11-06; fi\n")
			write(filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
			write(filepath.Join(bin, "sleep"), `#!/bin/sh
case "$SCENARIO:$TICK" in
  first-interrupted*:1) [ ! -e "$FAKE_DIR/deployed" ] || kill -TERM "$PPID";;
esac
exit 0
`)
			write(filepath.Join(bin, "docker"), `#!/bin/sh
echo "$*" >> "$FAKE_DIR/calls"
old=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
new=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
next=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
broken=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
unclean=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
repo=ghcr.io/menand/antispambot
active=old
if [ -e "$FAKE_DIR/deployed" ]; then active=$(cat "$FAKE_DIR/deployed"); fi
if [ "$SCENARIO" = containerd-unchanged ]; then active=new; fi
if [ -e "$FAKE_DIR/rollback" ] && [ "$SCENARIO" != rollback-wrong-image ]; then active=old; fi
case "$1 $2" in
  'inspect --format')
    case "$3" in
      '{{.Image}}')
        case "$SCENARIO" in first-*) [ -e "$FAKE_DIR/deployed" ] || exit 1;; esac
        if [ "$SCENARIO:$TICK" = retention:3 ] && [ ! -e "$FAKE_DIR/reapplied" ]; then exit 1; fi
        [ ! -e "$FAKE_DIR/stopped" ] || exit 1
        case "$SCENARIO" in containerd-*) echo "sha256:config-$active";; *) echo "sha256:$active";; esac;;
      *org.opencontainers.image.revision*) echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa;;
      *com.docker.compose.project*) echo antispambot;;
      *io.menand.antispam.first-release*) cat "$FAKE_DIR/instance-label" 2>/dev/null;;
      *Mounts*) if [ "$SCENARIO:$TICK" = first-interrupted-wrong-volume:2 ]; then echo other_volume; else echo antispambot_bot-data; fi;;
      *ImageManifestDescriptor*platform*)
        case "$SCENARIO" in containerd-*) echo linux/amd64;; esac;;
      *ImageManifestDescriptor*)
        case "$SCENARIO" in containerd-*) if [ "$active" = old ]; then echo "sha256:$old"; else echo "sha256:$new"; fi;; esac;;
      '{{.Id}} {{.State.StartedAt}}')
        if [ "$SCENARIO:$TICK" = first-interrupted-replaced:2 ]; then echo "$active 2026-11-19T17:01:00.123456789Z"; else echo "$active 2026-11-19T17:00:00.123456789Z"; fi;;
      '{{.State.Status}}') [ ! -e "$FAKE_DIR/stopped" ] && echo running;;
      '{{.RestartCount}}')
        if [ "$SCENARIO" = early-restart ] && [ ! -e "$FAKE_DIR/rollback" ]; then echo 1; else echo 0; fi;;
    esac;;
  'image inspect')
    if [ "$3" = --platform ]; then
      if [ "$7" = sha256:index-old ]; then echo "sha256:$old"; else echo "sha256:$new"; fi
      exit 0
    fi
    [ "$3" = --format ] || exit 0
    case "$5" in
      *config-*) exit 1;;
      *"$old"|sha256:old) target=old; value=$old;;
      *"$next") target=next; value=$next;;
      *"$broken") target=broken; value=$broken;;
      *) target=new; value=$new;;
    esac
    if [ "$SCENARIO" = unchanged ]; then target=old; value=$old; fi
    if [ "$5" = "$repo:latest" ]; then
      case "$SCENARIO:$TICK" in
        first-interrupted-new-tag:2|first-interrupted-unhealthy:3|retention:2|retention:3) target=next; value=$next;;
        retention:4|retention:5) target=broken; value=$broken;;
      esac
    fi
    case "$4" in
      *RepoDigests*) echo "$repo@sha256:$value";;
      *org.opencontainers.image.revision*) printf '%s\n' "$value" | cut -c 1-40;;
      *Config.Labels*) exit 0;;
      *) case "$SCENARIO" in containerd-*) echo "sha256:index-$target";; *) echo "sha256:$target";; esac;;
    esac;;
  "pull $repo:latest")
    case "$SCENARIO" in notification-failed|pull-failed) exit 1;; esac;;
  'compose --project-name')
    [ "$3" = antispambot ] || exit 2
    [ "$5" = "$FAKE_DIR" ] && [ "$7" = "$FAKE_DIR/.env" ] || exit 2
    [ "$8" = -f ] && [ "${10}" = -f ] && [ "${12}" = up ] && [ "${17}" = never ] || exit 2
    case "$SCENARIO" in first-*) ;; *)
      grep -Fx '    name: "antispambot_bot-data"' "${11}" >/dev/null || exit 2
    esac
    spec=$(cat "$9")
    echo "$spec" >> "$FAKE_DIR/applied"
    if [ "$spec" = 'previous compose' ]; then
      grep -F "$repo@sha256:$old" "${11}" >/dev/null || exit 2
      [ "$SCENARIO" != rollback-up-failed ] || exit 1
      touch "$FAKE_DIR/rollback"
      rm -f "$FAKE_DIR/stopped"
      exit 0
    fi
    case "$spec" in
      'candidate compose') digest=$new; active_name=new;; 'next compose') digest=$next; active_name=next;;
      'broken compose') digest=$broken; active_name=broken;; *) exit 2;;
    esac
    grep -F "$repo@sha256:$digest" "${11}" >/dev/null || exit 2
    if [ "$SCENARIO" = up-failed ] || [ "$SCENARIO" = first-failed ]; then
      touch "$FAKE_DIR/stopped"
      exit 1
    fi
    case "$SCENARIO" in up-noop|containerd-up-noop) ;; *) echo "$active_name" > "$FAKE_DIR/deployed";; esac
    if [ "$SCENARIO:$TICK" = retention:3 ]; then touch "$FAKE_DIR/reapplied"; fi
    sed -n 's/.*io.menand.antispam.first-release: "\(.*\)"/\1/p' "${11}" > "$FAKE_DIR/instance-label"
    if [ "$SCENARIO:$TICK" = first-interrupted-before-inspect:1 ]; then kill -TERM "$PPID"; fi;;
  'exec broken') exit 1;;
  'exec new'|'exec old'|'exec next')
    [ "$3" = sh ] && [ "$7" = 2000000000 ] || exit 2
    if [ -e "$FAKE_DIR/rollback" ]; then
      [ "$SCENARIO" != rollback-unhealthy ]
    else
      case "$SCENARIO" in unhealthy|previous-heartbeat|rollback-unhealthy|rollback-wrong-image|rollback-up-failed) exit 1;; esac
      [ "$SCENARIO:$TICK" != first-interrupted-unhealthy:2 ]
    fi;;
  'ps -aq')
    if [ "$SCENARIO:$TICK" = retention:2 ] && [ "$4" = "ancestor=$repo@sha256:$old" ]; then echo other-project-container; fi;;
  'image ls') printf '%s\n' "$repo@sha256:$old" "$repo@sha256:$new" "$repo@sha256:$next" "$repo@sha256:$broken" "$repo@sha256:$unclean";;
  'image rm')
    [ "$3" = "$repo@sha256:$old" ] && [ "$TICK" = 3 ] || {
      [ "$3" = "$repo@sha256:$unclean" ] || exit 2
      exit 1
    };;
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
			cmd.Env = []string{"PATH=/usr/bin:/bin", "FAKE_DIR=" + dir, "SCENARIO=" + scenario, "COMPOSE_PROJECT_NAME=antispambot", "TICK=1"}
			out, runErr := cmd.CombinedOutput()
			callsRaw, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil && (scenario != "fetch-failed" || !os.IsNotExist(err)) {
				t.Fatalf("no Docker calls: %v\n%s", err, out)
			}
			calls := string(callsRaw)
			if strings.Contains(calls, "image prune") || strings.Contains(calls, "\ntag ") {
				t.Fatal("deploy must not prune other projects' images")
			}
			if strings.HasPrefix(scenario, "first-interrupted") {
				checkInterruptedFirstDeploy(t, dir, bin, scenario, cmd.Env, calls, out, runErr)
				return
			}
			if scenario == "retention" {
				checkDeployRetention(t, dir, bin, cmd.Env, runErr)
				return
			}
			switch scenario {
			case "unchanged", "containerd-unchanged":
				if runErr != nil || strings.Contains(calls, "compose --project-name") || strings.Contains(calls, "image rm") || len(out) != 0 {
					t.Fatalf("unchanged image must leave the container alone and stay silent: %v\n%s\n%s", runErr, out, calls)
				}
				if _, err := os.Stat(filepath.Join(dir, "notifications")); !os.IsNotExist(err) {
					t.Fatal("unchanged image must not call Telegram")
				}
			case "success", "tag-conflict", "first-success":
				if runErr != nil || strings.Count(calls, "compose --project-name") != 1 || strings.Contains(calls, "image rm") {
					t.Fatalf("successful deploy must retain old image and apply one Compose bundle: %v\n%s\n%s", runErr, out, calls)
				}
			case "containerd-success":
				if runErr != nil || strings.Count(calls, "compose --project-name") != 1 || strings.Contains(calls, "image rm") {
					t.Fatalf("new platform manifest must deploy successfully: %v\n%s\n%s", runErr, out, calls)
				}
				if !strings.Contains(calls, "image inspect --platform linux/amd64 --format {{.Id}} sha256:index-new") {
					t.Fatal("manifest comparison must use the pulled immutable image and the container's platform")
				}
			case "fetch-failed", "pull-failed":
				if runErr == nil || strings.Contains(calls, "compose --project-name") {
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
			case "up-failed", "unhealthy", "previous-heartbeat", "early-restart", "up-noop", "containerd-up-noop", "rollback-unhealthy", "rollback-wrong-image", "rollback-up-failed":
				applied, err := os.ReadFile(filepath.Join(dir, "applied"))
				if runErr == nil || err != nil || string(applied) != "candidate compose\nprevious compose\n" || strings.Count(calls, "compose --project-name") != 2 {
					t.Fatalf("failed deploy must attempt rollback using the saved bundle: %v / %v\n%s\n%s", runErr, err, out, calls)
				}
				if scenario == "up-failed" {
					pointer, err := os.ReadFile(filepath.Join(dir, "releases", "current"))
					if err != nil {
						t.Fatal(err)
					}
					digest, err := os.ReadFile(filepath.Join(dir, "releases", strings.TrimSpace(string(pointer)), "image"))
					if err != nil || strings.TrimSpace(string(digest)) != "ghcr.io/menand/antispambot@sha256:"+strings.Repeat("a", 64) {
						t.Fatalf("rollback changed the confirmed release: %v / %s", err, digest)
					}
					// Simulate the next cron tick with the same admitted image and a newer,
					// unverified checkout script. It must keep the restored container.
					write(filepath.Join(dir, "scripts", "deploy-release.sh"), "echo unverified >> \"$FAKE_DIR/controllers\"\n")
					next := exec.CommandContext(t.Context(), "bash", filepath.Join(bin, "auto-deploy.sh"))
					next.Env = cmd.Env
					if out, err := next.CombinedOutput(); err != nil || len(out) != 0 {
						t.Fatalf("quarantined release was processed again: %v / %s", err, out)
					}
					controllers, err := os.ReadFile(filepath.Join(dir, "controllers"))
					if err != nil || string(controllers) != "admitted\n" {
						t.Fatalf("executed an unexpected controller: %v / %s", err, controllers)
					}
				}
				if strings.Contains(calls, "image rm") {
					t.Fatal("failed deploy must keep the rollback image")
				}
				if scenario == "previous-heartbeat" && !strings.Contains(string(out), "текущий экземпляр не подтвердил готовность за 120 секунд") {
					t.Fatalf("an old heartbeat must not confirm the candidate: %s", out)
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

func checkDeployRetention(t *testing.T, dir, bin string, env []string, firstErr error) {
	t.Helper()
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	state := filepath.Join(dir, "releases")
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	ref := func(letter string) string { return "ghcr.io/menand/antispambot@sha256:" + strings.Repeat(letter, 64) }
	assertPointer := func(name, image string) {
		t.Helper()
		if got := read(filepath.Join(state, read(filepath.Join(state, name)), "image")); got != ref(image) {
			t.Fatalf("pointer %s = %s", name, got)
		}
	}
	assertPointer("current", "b")
	assertPointer("previous", "a")
	firstBundle := read(filepath.Join(state, "previous"))
	for _, b := range []struct{ name, project, digest string }{
		{"bundle.duplicate", "antispambot", "c"}, {"bundle.pending", "antispambot", "f"},
		{"bundle.unclean", "antispambot", "e"}, {"bundle.foreign", "different", "9"},
	} {
		bundle := filepath.Join(state, b.name)
		if err := os.Mkdir(bundle, 0o700); err != nil {
			t.Fatal(err)
		}
		for file, value := range map[string]string{"image": ref(b.digest), "project": b.project, "volume": "antispambot_bot-data"} {
			if err := os.WriteFile(filepath.Join(bundle, file), []byte(value+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(state, "pending"), []byte("bundle.pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(tick string) ([]byte, error) {
		t.Helper()
		next := exec.CommandContext(t.Context(), "bash", filepath.Join(bin, "auto-deploy.sh"))
		next.Env = append(append([]string(nil), env[:len(env)-1]...), "TICK="+tick)
		return next.CombinedOutput()
	}
	out, err := run("2")
	if err != nil || !strings.Contains(string(out), "cleanup") {
		t.Fatalf("cleanup failure must leave the release confirmed: %v / %s", err, out)
	}
	assertPointer("current", "c")
	assertPointer("previous", "b")
	assertPointer("pending", "f")
	firstC := read(filepath.Join(state, "current"))
	if calls := read(filepath.Join(dir, "calls")); strings.Contains(calls, "image rm "+ref("a")) || strings.Contains(calls, "image rm "+ref("c")) {
		t.Fatalf("removed the digest of a live container or a retained bundle: %s", calls)
	}
	if _, err := os.Stat(filepath.Join(state, "bundle.duplicate")); !os.IsNotExist(err) {
		t.Fatalf("redundant bundle was retained: %v", err)
	}
	if out, err := run("3"); err != nil {
		t.Fatalf("reapplying the same digest failed: %v / %s", err, out)
	}
	assertPointer("current", "c")
	assertPointer("previous", "b")
	assertPointer("pending", "f")
	for _, stale := range []string{firstBundle, firstC} {
		if _, err := os.Stat(filepath.Join(state, stale)); !os.IsNotExist(err) {
			t.Fatalf("obsolete bundle %s was retained: %v", stale, err)
		}
	}
	if out, err := run("4"); err == nil || !strings.Contains(string(out), "rollback done") {
		t.Fatalf("rollback failed: %v / %s", err, out)
	}
	assertPointer("current", "c")
	assertPointer("previous", "b")
	if out, err := run("5"); err != nil || len(out) != 0 {
		t.Fatalf("quarantine did not block retry: %v / %s", err, out)
	}
	calls := read(filepath.Join(dir, "calls"))
	if strings.Contains(calls, "image prune") || strings.Contains(calls, "system prune") || strings.Count(calls, "image rm "+ref("a")) != 1 {
		t.Fatalf("unexpected cleanup commands: %s", calls)
	}
	for _, kept := range []string{"b", "c", "d", "f", "9"} {
		if strings.Contains(calls, "image rm "+ref(kept)) {
			t.Fatalf("removed a protected digest %s", kept)
		}
	}
	if got := read(filepath.Join(dir, "applied")); got != "candidate compose\nnext compose\nnext compose\nbroken compose\nnext compose" {
		t.Fatalf("unexpected apply/rollback sequence: %s", got)
	}
	if got := read(filepath.Join(state, "bundle.foreign", "project")); got != "different" {
		t.Fatalf("unrelated project was changed: %s", got)
	}
}

func checkInterruptedFirstDeploy(t *testing.T, dir, bin, scenario string, env []string, calls string, out []byte, runErr error) {
	t.Helper()
	if runErr == nil || !strings.Contains(calls, "compose --project-name") {
		t.Fatalf("first tick was not interrupted after Compose: %v / %s / %s", runErr, out, calls)
	}
	state := filepath.Join(dir, "releases")
	if _, err := os.Stat(filepath.Join(state, "current")); !os.IsNotExist(err) {
		t.Fatalf("interrupted release became current: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "pending")); err != nil {
		t.Errorf("missing pending bundle after interrupted tick: %v", err)
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	run := func(tick string) ([]byte, error) {
		t.Helper()
		next := exec.CommandContext(t.Context(), "bash", filepath.Join(bin, "auto-deploy.sh"))
		next.Env = append(append([]string(nil), env[:len(env)-1]...), "TICK="+tick)
		return next.CombinedOutput()
	}
	assertCurrent := func(digest string) {
		t.Helper()
		bundle := strings.TrimSpace(read(filepath.Join(state, "current")))
		if got := strings.TrimSpace(read(filepath.Join(state, bundle, "image"))); got != "ghcr.io/menand/antispambot@sha256:"+strings.Repeat(digest, 64) {
			t.Fatalf("unexpected confirmed digest: %s", got)
		}
	}
	out, err := run("2")
	updated := strings.TrimPrefix(read(filepath.Join(dir, "calls")), calls)
	if err == nil && (scenario == "first-interrupted" || scenario == "first-interrupted-before-inspect" || scenario == "first-interrupted-new-tag") {
		assertCurrent("b")
		if strings.Contains(updated, "compose --project-name") || strings.Contains(updated, "pull ") || !strings.Contains(updated, "exec new ") {
			t.Fatalf("resume must smoke the original instance without Compose: %s", updated)
		}
		return
	}
	if err == nil || !strings.Contains(string(out), "no previous image") {
		t.Fatalf("resumed tick neither confirmed the expected healthy instance nor reported failure: %v / %s / %s", err, out, updated)
	}
	if _, err := os.Stat(filepath.Join(state, "current")); !os.IsNotExist(err) {
		t.Fatalf("unhealthy first instance became a rollback target: %v", err)
	}
	if scenario == "first-interrupted-unhealthy" {
		out, err = run("3")
		if err != nil || read(filepath.Join(dir, "applied")) != "candidate compose\nnext compose\n" {
			t.Fatalf("new tag did not replace the unconfirmed first bundle: %v / %s", err, out)
		}
		assertCurrent("c")
	}
}
