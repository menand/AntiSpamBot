package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestDeployNotification(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(subject string) string {
		t.Helper()
		git("add", "CHANGELOG.md")
		git("-c", "user.name=Deploy Test", "-c", "user.email=deploy-test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", subject)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q")
	write(filepath.Join(dir, "CHANGELOG.md"), "## v1.0.0 — 2026-11-01\n- Прежняя версия.\n")
	oldRevision := commit("Previous release")
	write(filepath.Join(dir, "CHANGELOG.md"), "## v1.1.0 — 2026-11-07\n- Исправлена капча.\n- Проверен откат.\n- Улучшены уведомления.\n- Обновлена база.\n- Остальные изменения.\n\n## v1.0.0 — 2026-11-01\n- Прежняя версия.\n")
	newRevision := commit("Fix captcha lifecycle")
	write(filepath.Join(dir, "CHANGELOG.md"), "## v9.0.0 — 2026-11-08\n- ЕЩЁ НЕ УСТАНОВЛЕНО.\n")
	commit("Future release still building")
	// Deliberately keep HEAD ahead of the image. Its release notes must not
	// appear in the notification about the older, actually deployed revision.
	helper, err := filepath.Abs("deploy-notify.sh")
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(bin, "docker"), `#!/bin/sh
if [ "$1" = run ]; then
  printf '%s\n' "$*" >> "$DOCKER_CALLS"
  for arg in "$@"; do
    case "$arg" in sha256:old) printf '%s\n' "$OLD_ALPINE_VERSION";; sha256:new) printf '%s\n' "$NEW_ALPINE_VERSION";; esac
  done
  exit 0
fi
case "$5" in
  sha256:old) version=$OLD_VERSION; revision=$OLD_REVISION; golang=old-go; alpine=$OLD_ALPINE_DIGEST;;
  *) version=$NEW_VERSION; revision=$NEW_REVISION; golang=$NEW_GOLANG; alpine=$NEW_ALPINE_DIGEST;;
esac
case "$4" in
  *org.opencontainers.image.version*) printf '%s' "$version";;
  *org.opencontainers.image.revision*) printf '%s' "$revision";;
  *base.digest.golang*) printf '%s' "$golang";;
  *base.digest.alpine*) printf '%s' "$alpine";;
esac
`)
	write(filepath.Join(bin, "timeout"), "#!/bin/sh\nshift\nexec \"$@\"\n")
	for _, scenario := range []string{"release", "base-rebuild", "same-code", "unknown", "commit-only", "getme-failed", "long-notes", "alpine-upgrade", "alpine-repack", "alpine-old-unknown", "alpine-new-unknown", "alpine-invalid"} {
		t.Run(scenario, func(t *testing.T) {
			oldVersion, newVersion := "v1.0.0", "v1.1.0"
			oldRev, newRev := oldRevision, newRevision
			newGo := "old-go"
			apiOK := "1"
			oldAlpineDigest, newAlpineDigest := "same-alpine", "same-alpine"
			oldAlpineVersion, newAlpineVersion := "3.24.4", "3.24.5"
			switch scenario {
			case "base-rebuild", "same-code":
				oldRev = newRev
				oldVersion = newVersion
				if scenario == "base-rebuild" {
					newGo = "new-go"
				}
			case "unknown":
				oldVersion, newVersion, oldRev, newRev = "", "", "", ""
			case "commit-only":
				oldVersion, newVersion = "v1.0.0-1-gabcdef0", "v1.0.0-2-g1234567"
			case "getme-failed":
				apiOK = "0"
			case "alpine-upgrade", "alpine-repack", "alpine-old-unknown", "alpine-new-unknown", "alpine-invalid":
				oldRev = newRev
				oldVersion = newVersion
				oldAlpineDigest, newAlpineDigest = "old-base", "new-base"
				if scenario == "alpine-repack" {
					newAlpineVersion = oldAlpineVersion
				}
				if scenario == "alpine-old-unknown" {
					oldAlpineVersion = ""
					oldAlpineDigest = ""
				}
				if scenario == "alpine-new-unknown" {
					newAlpineVersion = ""
				}
				if scenario == "alpine-invalid" {
					newAlpineVersion = "3.24.5&unexpected=1"
				}
			}
			body := `set -euo pipefail
source "$DEPLOY_HELPER"
old_version=$(image_version sha256:old)
new_version=$(image_version sha256:new)
old_revision=$(image_revision sha256:old)
new_revision=$(image_revision sha256:new)
changes=$(deployment_changes sha256:old sha256:new "$old_version" "$new_version" "$old_revision" "$new_revision")
alpine_info=$(alpine_details sha256:old sha256:new)
deployment_notice "$old_version" "$new_version" sha256:new "$changes" "$API_OK" "$alpine_info"
`
			if scenario == "long-notes" {
				body = `set -euo pipefail
source "$DEPLOY_HELPER"
changes=$(printf '%s\n' "$LONG_NOTES" | bounded_notes)
deployment_notice v1.0.0 v1.1.0 sha256:new "$changes" 1
`
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-c", body)
			cmd.Dir = dir
			cmd.Env = []string{
				"PATH=" + bin + ":/usr/bin:/bin", "DEPLOY_HELPER=" + helper,
				"OLD_VERSION=" + oldVersion, "NEW_VERSION=" + newVersion,
				"OLD_REVISION=" + oldRev, "NEW_REVISION=" + newRev,
				"NEW_GOLANG=" + newGo, "API_OK=" + apiOK,
				"OLD_ALPINE_DIGEST=" + oldAlpineDigest, "NEW_ALPINE_DIGEST=" + newAlpineDigest,
				"OLD_ALPINE_VERSION=" + oldAlpineVersion, "NEW_ALPINE_VERSION=" + newAlpineVersion,
				"DOCKER_CALLS=" + filepath.Join(dir, "docker-calls-"+scenario),
				"LONG_NOTES=- " + strings.Repeat("😀", 1200) + "\n- Короткий пункт.\n- Ещё один пункт.",
				"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("notification failed: %v\n%s", err, out)
			}
			text := string(out)
			if !utf8.Valid(out) || len(utf16.Encode([]rune(text))) > 4096 {
				t.Fatal("notification must be valid UTF-8 and fit Telegram's UTF-16 limit")
			}
			if strings.Contains(text, "ЕЩЁ НЕ УСТАНОВЛЕНО") {
				t.Fatal("notification used checkout HEAD instead of the deployed image revision")
			}
			switch scenario {
			case "release", "getme-failed":
				if !strings.Contains(text, "v1.0.0 → v1.1.0") || !strings.Contains(text, "Исправлена капча") || !strings.Contains(text, "/whatsnew") || strings.Contains(text, "Прежняя версия") {
					t.Fatalf("notification must describe the installed release and bound its notes: %s", text)
				}
				if scenario == "getme-failed" && (strings.Contains(text, "✅") || !strings.Contains(text, "Telegram API недоступен")) {
					t.Fatalf("Telegram outage must not be reported as full success: %s", text)
				}
			case "base-rebuild":
				if !strings.Contains(text, "обновлены базовые образы (golang)") || !strings.Contains(text, "Код той же версии") || strings.Contains(text, "Исправлена капча") {
					t.Fatalf("base rebuild must not announce old application changes again: %s", text)
				}
			case "same-code":
				if !strings.Contains(text, "ревизия кода не изменилась") || strings.Contains(text, "обновлены базовые образы") {
					t.Fatalf("unknown rebuild cause must not be invented: %s", text)
				}
			case "unknown":
				if !strings.Contains(text, "неизвестна → неизвестна") || !strings.Contains(text, "нет метаданных") || strings.Contains(text, "v9.0.0") {
					t.Fatalf("legacy image must not borrow a version from checkout HEAD: %s", text)
				}
			case "commit-only":
				if !strings.Contains(text, "Fix captcha lifecycle") || strings.Contains(text, "Прежняя версия") {
					t.Fatalf("untagged changes must use image revisions: %s", text)
				}
			case "long-notes":
				if !strings.Contains(text, "Короткий пункт") || !strings.Contains(text, "/whatsnew") {
					t.Fatalf("oversized Unicode note must leave a readable bounded summary: %s", text)
				}
			case "alpine-upgrade", "alpine-old-unknown":
				old := "3.24.4"
				if scenario == "alpine-old-unknown" {
					old = "неизвестна"
				}
				if !strings.Contains(text, "Alpine: "+old+" → 3.24.5") || !strings.Contains(text, "https://git.alpinelinux.org/aports/log/?h=v3.24.5") {
					t.Fatalf("Alpine upgrade must show actual image versions and official release history: %s", text)
				}
				calls, err := os.ReadFile(filepath.Join(dir, "docker-calls-"+scenario))
				if err != nil || !strings.Contains(string(calls), "run --rm --pull=never --network=none --read-only --tmpfs /data --entrypoint /bin/cat sha256:new /etc/alpine-release") {
					t.Fatalf("version probe must not use network or the bot's data volume: %v\n%s", err, calls)
				}
			case "alpine-repack":
				if !strings.Contains(text, "Alpine: 3.24.4 → 3.24.4") || !strings.Contains(text, "Номер выпуска совпадает") {
					t.Fatalf("repacked image must not invent a release transition: %s", text)
				}
			case "alpine-new-unknown", "alpine-invalid":
				if !strings.Contains(text, "Alpine: 3.24.4 → неизвестна") || !strings.Contains(text, "https://alpinelinux.org/releases/") || strings.Contains(text, "unexpected=1") {
					t.Fatalf("unavailable or malformed version must use safe fallback: %s", text)
				}
			}
		})
	}
}
