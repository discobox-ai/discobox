package provision

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

func TestGoInstallTruncatedArchiveDoesNotPublishPartialToolchain(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux installer")
	}
	script, err := os.ReadFile("../features/go/install-go.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	// Redirect only the installation prefix; execute the real installer and tar.
	prefix := filepath.Join(root, "local")
	if err := os.MkdirAll(prefix, 0755); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	data := []byte("#!/bin/sh\necho incomplete-toolchain\n")
	if err := tw.WriteHeader(&tar.Header{Name: "go/bin/go", Mode: 0755, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	// Advertise a second file, but end the compressed stream before its contents.
	if err := tw.WriteHeader(&tar.Header{Name: "go/src/runtime/runtime.go", Mode: 0644, Size: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "truncated.tar.gz")
	if err := os.WriteFile(payload, archive.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	curl := "#!/bin/bash\ncase \"$*\" in *VERSION*) echo go1.99.0 ;; *) cat \"$TEST_GO_ARCHIVE\" ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", strings.ReplaceAll(string(script), "/usr/local", prefix)) // #nosec G204 -- Repository installer and test-owned prefix only.
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_GO_ARCHIVE="+payload)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("truncated archive unexpectedly succeeded: %s", output)
	}
	if !strings.Contains(string(output), "Unexpected EOF") {
		t.Fatalf("failed for another reason: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(prefix, "go", "bin", "go")); !os.IsNotExist(err) {
		t.Fatalf("failed install published a partial go/bin/go (stat error: %v); installer output:\n%s", err, output)
	}
}

func TestGoInstallRecoveryAndPreservation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux installer")
	}
	for _, recover := range []bool{false, true} {
		t.Run(map[bool]string{false: "exhausted", true: "recovers"}[recover], func(t *testing.T) {
			root := t.TempDir()
			prefix := filepath.Join(root, "local")
			bin := filepath.Join(root, "bin")
			for _, dir := range []string{filepath.Join(prefix, "go", "bin"), bin} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			old := filepath.Join(prefix, "go", "bin", "go")
			if err := os.WriteFile(old, []byte("old-toolchain"), 0755); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			data := []byte("#!/bin/sh\necho go version go1.99.0 linux/amd64\n")
			if err := tw.WriteHeader(&tar.Header{Name: "go/bin/go", Mode: 0755, Size: int64(len(data))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(root, "complete.tar.gz")
			if err := os.WriteFile(payload, archive.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
			curl := `#!/bin/bash
case "$*" in *VERSION*) echo go1.99.0; exit;; esac
n=0
[[ ! -f "$TEST_ATTEMPTS" ]] || n=$(cat "$TEST_ATTEMPTS")
echo $((n+1)) > "$TEST_ATTEMPTS"
if [[ "$TEST_RECOVER" == true && "$n" -gt 0 ]]; then
    cat "$TEST_GO_ARCHIVE"
else
    head -c 20 "$TEST_GO_ARCHIVE"
fi
`
			if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0755); err != nil {
				t.Fatal(err)
			}
			script, err := os.ReadFile("../features/go/install-go.sh")
			if err != nil {
				t.Fatal(err)
			}
			attempts := filepath.Join(root, "attempts")
			b := newBox(t)
			b.feature("go", map[string]string{
				provisionspec.FileName: asyncHead + "steps:\n  - name: install\n    run: install-go.sh\n",
				"install-go.sh":        strings.ReplaceAll(string(script), "/usr/local", prefix),
			})
			tracker := b.tracked()
			b.runner.Command = func(ctx context.Context, out io.Writer, dir string, env []string, name string, args ...string) error {
				env = append(env, "PATH="+bin+":"+os.Getenv("PATH"), "TEST_GO_ARCHIVE="+payload, "TEST_ATTEMPTS="+attempts, "TEST_RECOVER="+map[bool]string{true: "true", false: "false"}[recover])
				return runCommand(ctx, out, dir, env, name, args...)
			}
			_, err = b.apply()
			output := b.read(filepath.Join(LogDir, "go.log"))
			if (err == nil) != recover {
				t.Fatalf("recover=%v: %v: %s", recover, err, output)
			}
			if err := tracker.AwaitReady(context.Background()); err != nil {
				t.Fatalf("background failure blocked readiness: %v", err)
			}
			prerequisiteErr := tracker.AwaitFeatures(context.Background(), nil)
			feature := tracker.Status().Features["go"]
			if recover {
				if prerequisiteErr != nil || feature.State != provisionspec.StateReady {
					t.Fatalf("service prerequisite: %v; feature: %+v", prerequisiteErr, feature)
				}
			} else {
				if prerequisiteErr == nil || !strings.Contains(prerequisiteErr.Error(), "go.log") || feature.State != provisionspec.StateFailed || !strings.Contains(feature.Error, `step "install"`) {
					t.Fatalf("service prerequisite: %v; feature: %+v", prerequisiteErr, feature)
				}
				if !strings.Contains(output, "failed after 3 attempts; retry provisioning") {
					t.Fatalf("missing actionable failure: %s", output)
				}
			}
			count, err := os.ReadFile(attempts)
			if err != nil {
				t.Fatal(err)
			}
			wantCount := "3\n"
			if recover {
				wantCount = "2\n"
			}
			if string(count) != wantCount {
				t.Fatalf("attempts %q, want %q: %s", count, wantCount, output)
			}
			installed, err := os.ReadFile(old)
			if err != nil {
				t.Fatal(err)
			}
			want := "old-toolchain"
			if recover {
				want = string(data)
			}
			if string(installed) != want {
				t.Fatalf("toolchain = %q, want %q", installed, want)
			}
			staging, err := filepath.Glob(filepath.Join(prefix, ".go-install.*"))
			if err != nil || len(staging) != 0 {
				t.Fatalf("staging left: %v, %v", staging, err)
			}
			if !strings.Contains(output, "attempt 1/3") {
				t.Fatalf("missing retry diagnostic: %s", output)
			}
		})
	}
}
