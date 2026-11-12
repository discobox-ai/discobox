package devbuild_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Exercise the commands the watchers actually use, with small real Go modules.
func TestWatcherPublication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell and signals")
	}
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []string{".wnb.yaml", ".wnb.cli.yaml"} {
		for _, suffix := range []string{"", ".exe"} {
			t.Run(config+suffix, func(t *testing.T) {
				dir := t.TempDir()
				write := func(name, content string, mode os.FileMode) {
					t.Helper()
					path := filepath.Join(dir, name)
					if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(content), mode); err != nil {
						t.Fatal(err)
					}
				}
				for _, module := range []string{"cli", "server"} {
					write(module+"/go.mod", "module fixture/"+module+"\n\ngo 1.25\n", 0600)
					command := "discobox"
					if module == "server" {
						command += "-server"
					}
					write(module+"/cmd/"+command+"/main.go", "package main\nfunc main() { println(\"ok\") }\n", 0600)
				}
				script, err := os.ReadFile(filepath.Join(root, "scripts/dev-build.sh"))
				if err != nil {
					t.Fatal(err)
				}
				write("scripts/dev-build.sh", string(script), 0700)
				// Intercept signing and inject failures after writing partial staged output.
				// Successful builds still use the real compiler, including its build cache.
				write("bin/go", `#!/usr/bin/env bash
set -eu
if [ "$1" = env ] && [ "$2" = GOEXE ]; then
 printf '%s\n' "$TEST_EXE"
 exit
fi
if [ "$1" = tool ]; then
 test "${FAIL_AT:-}" != sign
 exit
fi
if [ "$1" = build ] && [ -n "${FAIL_AT:-}" ] && [ "$FAIL_AT" != sign ] && [ "$FAIL_AT" != publish ] && [ "$FAIL_AT" != retire ]; then
 while [ "$1" != -o ]; do shift; done
 printf partial > "$2"
 if [ "$FAIL_AT" = cancel ]; then kill -TERM "$PPID"; fi
 exit 1
fi
exec "$REAL_GO" "$@"
`, 0700)
				realMV, err := exec.LookPath("mv")
				if err != nil {
					t.Fatal(err)
				}
				write("bin/mv", `#!/usr/bin/env bash
set -eu
args=("$@")
src=${args[${#args[@]}-2]}
dst=${args[${#args[@]}-1]}
# Model Windows rejecting replacement of a mapped executable.
if [[ "$dst" = build/*.exe && -f "$dst" ]]; then exit 1; fi
if [[ "$src" = build/.dev-build.*/* && "$FAIL_AT" = publish ]]; then exit 1; fi
"$REAL_MV" "$@"
if [[ "$dst" = build/.dev-retiring-* && "$FAIL_AT" = retire ]]; then
 kill -TERM "$PPID"
fi
`, 0700)
				data, err := os.ReadFile(filepath.Join(root, config))
				if err != nil {
					t.Fatal(err)
				}
				var cfg struct {
					Build struct {
						Command string `yaml:"command"`
					} `yaml:"build"`
				}
				if err := yaml.Unmarshal(data, &cfg); err != nil {
					t.Fatal(err)
				}
				build := func(failure string) error {
					t.Helper()
					// The repository-owned watcher config is the entrypoint under test.
					cmd := exec.CommandContext(t.Context(), "bash", "-c", cfg.Build.Command) //nolint:gosec // Execute the repository-owned watcher command.
					cmd.Dir = dir
					cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "REAL_GO="+realGo, "GOWORK=off", "FAIL_AT="+failure, "TEST_EXE="+suffix, "REAL_MV="+realMV)
					output, err := cmd.CombinedOutput()
					if err != nil {
						t.Logf("build (%s): %v\n%s", failure, err, output)
					}
					return err
				}
				if err := build(""); err != nil {
					t.Fatal(err)
				}
				targets := []string{"discobox"}
				if config == ".wnb.yaml" {
					targets = append(targets, "discobox-server")
				}
				for i := range targets {
					targets[i] += suffix
				}
				originals := map[string]string{}
				for _, name := range targets {
					data, err := os.ReadFile(filepath.Join(dir, "build", name))
					if err != nil {
						t.Fatal(err)
					}
					originals[name] = string(data)
				}
				failures := []string{"compile", "cancel", "sign"}
				if suffix == ".exe" {
					failures = append(failures, "publish", "retire")
				}
				for _, failure := range failures {
					if failure == "sign" && config == ".wnb.cli.yaml" {
						continue
					}
					if err := build(failure); err == nil {
						t.Fatalf("%s unexpectedly succeeded", failure)
					}
					for name, want := range originals {
						data, err := os.ReadFile(filepath.Join(dir, "build", name))
						if err != nil || string(data) != want {
							t.Fatalf("%s changed live %s: %v", failure, name, err)
						}
					}
					leftovers, err := filepath.Glob(filepath.Join(dir, "build", ".dev-build.*"))
					if err != nil || len(leftovers) != 0 {
						t.Fatalf("staging cleanup: %v, %v", leftovers, err)
					}
				}
				for name, data := range originals {
					if err := os.Truncate(filepath.Join(dir, "build", name), int64(len(data)/2)); err != nil {
						t.Fatal(err)
					}
				}
				if err := build(""); err != nil {
					t.Fatal(err)
				}
				for name, want := range originals {
					path := filepath.Join(dir, "build", name)
					data, err := os.ReadFile(path)
					if err != nil || string(data) != want {
						t.Fatalf("successful rebuild left damaged %s: %v", name, err)
					}
					output, err := exec.CommandContext(t.Context(), path).CombinedOutput()
					if err != nil || strings.TrimSpace(string(output)) != "ok" {
						t.Fatalf("rebuilt %s: %v %s", name, err, output)
					}
				}
			})
		}
	}
}
