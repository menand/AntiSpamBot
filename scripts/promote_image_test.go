package scripts

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromoteImageSerialEligibility(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"base-digests.sh", "promote-image.sh"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "scripts")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(path, name), string(data))
	}
	write(filepath.Join(dir, "Dockerfile"), "FROM golang:1.27-alpine AS build\nFROM alpine:3.24\n")
	write(filepath.Join(bin, "git"), `#!/bin/bash
set -eu
case "$1" in
    rev-parse) echo "$GITHUB_SHA" ;;
    ls-remote) printf '%s\trefs/heads/main\n' "$(cat "$FIXTURES/main")" ;;
    *) exit 1 ;;
esac
`)
	write(filepath.Join(bin, "docker"), `#!/bin/bash
set -eu
case "$3" in
    inspect)
        case "$5" in
            golang:*) cat "$FIXTURES/golang.json" ;;
            alpine:*) cat "$FIXTURES/alpine.json" ;;
            *) exit 1 ;;
        esac
        ;;
    create)
        test "$4" = --prefer-index=false
        test "$5" = --tag
        test "$7" = --tag
        test "$8" = ghcr.io/menand/antispambot:latest
        printf '%s\n' "$9" > "$FIXTURES/latest"
        printf '%s\n' "$9" >> "$FIXTURES/publications"
        ;;
    *) exit 1 ;;
esac
`)

	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	setBase := func(name, hash string) {
		write(filepath.Join(dir, name+".json"), fmt.Sprintf(
			`{"manifests":[{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}}]}`, hash))
	}
	oldSHA, newSHA := strings.Repeat("1", 40), strings.Repeat("2", 40)
	golang, oldAlpine, newAlpine := digest("a"), digest("b"), digest("c")
	oldImage, newImage, rebuiltImage := digest("d"), digest("e"), digest("f")
	setBase("golang", golang)
	setBase("alpine", oldAlpine)
	write(filepath.Join(dir, "main"), newSHA)
	run := func(event, ref, rebuild, sha, alpine, image string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "bash", "scripts/promote-image.sh")
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=" + bin + ":/usr/bin:/bin", "FIXTURES=" + dir, "GITHUB_EVENT_NAME=" + event,
			"GITHUB_REF=" + ref, "REBUILD_IMAGE=" + rebuild, "GITHUB_SHA=" + sha,
			"CANDIDATE_GOLANG=" + golang, "CANDIDATE_ALPINE=" + alpine,
			"CANDIDATE_DIGEST=" + image, "CANDIDATE_VERSION=v1.0.0",
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("promote script: %v\n%s", err, out)
		}
	}
	latest := filepath.Join(dir, "latest")
	run("pull_request", "refs/pull/1/merge", "false", newSHA, oldAlpine, oldImage)
	run("workflow_dispatch", "refs/heads/debug", "true", newSHA, oldAlpine, oldImage)
	run("workflow_dispatch", "refs/heads/main", "false", newSHA, oldAlpine, oldImage)
	if _, err := os.Stat(latest); !os.IsNotExist(err) {
		t.Fatalf("unapproved runs touched latest: %v", err)
	}

	// Новая сборка завершилась первой, запоздалая не может сменить latest.
	run("push", "refs/heads/main", "false", newSHA, oldAlpine, newImage)
	run("push", "refs/heads/main", "false", oldSHA, oldAlpine, oldImage)

	// Та же ревизия и обновлённая база проходят; сборка со старой базой
	// в обратном порядке снова не может сменить latest.
	setBase("alpine", newAlpine)
	run("workflow_dispatch", "refs/heads/main", "true", newSHA, newAlpine, rebuiltImage)
	run("push", "refs/heads/main", "false", newSHA, oldAlpine, newImage)

	const prefix = "ghcr.io/menand/antispambot@"
	if data, err := os.ReadFile(latest); err != nil || string(data) != prefix+rebuiltImage+"\n" {
		t.Fatalf("latest = %q, %v, want rebuilt digest", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "publications")); err != nil ||
		string(data) != prefix+newImage+"\n"+prefix+rebuiltImage+"\n" {
		t.Fatalf("wrong publication order: %q, %v", data, err)
	}
}
