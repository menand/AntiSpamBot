package scripts

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaseDigestsSelectOnlyAMD64(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "Dockerfile"), "FROM golang:1.27-alpine AS build\nFROM alpine:3.24\n")
	write(filepath.Join(bin, "docker"), "#!/bin/sh\ncase \"$*\" in *golang*) cat \"$FIXTURES/go.json\";; *) cat \"$FIXTURES/alpine.json\";; esac\n")
	script, err := filepath.Abs("base-digests.sh")
	if err != nil {
		t.Fatal(err)
	}
	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	index := func(amd, other string) string {
		return fmt.Sprintf(`{"manifests":[{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}},{"digest":%q,"platform":{"os":"linux","architecture":"riscv64"}}]}`, amd, other)
	}
	write(filepath.Join(dir, "go.json"), index(digest("a"), digest("b")))
	run := func() (string, error) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "bash", script)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FIXTURES=" + dir}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	write(filepath.Join(dir, "alpine.json"), index(digest("c"), digest("d")))
	before, err := run()
	if err != nil {
		t.Fatalf("first index: %v\n%s", err, before)
	}
	write(filepath.Join(dir, "alpine.json"), index(digest("c"), digest("e")))
	after, err := run()
	if err != nil || before != after {
		t.Fatalf("RISC-V change must not alter amd64 output: %v\n%s\n%s", err, before, after)
	}
	write(filepath.Join(dir, "alpine.json"), index(digest("f"), digest("e")))
	changed, err := run()
	if err != nil || changed == after || !strings.Contains(changed, "alpine="+digest("f")) || !strings.Contains(changed, "alpine_context=alpine:3.24=docker-image://alpine:3.24@"+digest("f")) {
		t.Fatalf("amd64 change must select and pin the new manifest: %v\n%s", err, changed)
	}
	write(filepath.Join(dir, "alpine.json"), `{"manifests":[{"digest":"sha256:bad","platform":{"os":"linux","architecture":"riscv64"}}]}`)
	if out, err := run(); err == nil {
		t.Fatalf("missing amd64 must fail closed: %s", out)
	}
}
