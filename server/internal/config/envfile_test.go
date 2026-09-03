package config

import (
	"os"
	"path/filepath"
	"testing"
)

// testVar is set only by the files these tests write.
const testVar = "DISCOBOX_ENV_FILE_TEST"

// chdirWithEnvFile writes contents to name in a fresh directory and makes it the
// working directory, since LoadEnvFile resolves its file relative to that. The
// variable the file sets starts genuinely absent, the way it is in a fresh
// server process; t.Setenv is only there to register the restore.
func chdirWithEnvFile(t *testing.T, name, contents string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	t.Chdir(dir)
	t.Setenv(testVar, "")
	if err := os.Unsetenv(testVar); err != nil {
		t.Fatalf("unset %s: %v", testVar, err)
	}
}

func TestLoadEnvFileReadsDefaultFile(t *testing.T) {
	chdirWithEnvFile(t, EnvFile, testVar+"=from-file\n")

	LoadEnvFile()

	if got := os.Getenv(testVar); got != "from-file" {
		t.Fatalf("%s = %q, want %q", testVar, got, "from-file")
	}
}

// A stray .env is exactly what the default name exists to ignore: a hand-built
// binary run from a source tree must not be reconfigured by it.
func TestLoadEnvFileIgnoresDotEnv(t *testing.T) {
	chdirWithEnvFile(t, ".env", testVar+"=from-dot-env\n")

	LoadEnvFile()

	if got, ok := os.LookupEnv(testVar); ok {
		t.Fatalf("%s = %q, want unset", testVar, got)
	}
}

// How the development loop opts back in.
func TestLoadEnvFileReadsNamedFile(t *testing.T) {
	chdirWithEnvFile(t, ".env", testVar+"=from-dot-env\n")
	t.Setenv(EnvFileVar, ".env")

	LoadEnvFile()

	if got := os.Getenv(testVar); got != "from-dot-env" {
		t.Fatalf("%s = %q, want %q", testVar, got, "from-dot-env")
	}
}

func TestLoadEnvFileEmptyNameLoadsNothing(t *testing.T) {
	chdirWithEnvFile(t, EnvFile, testVar+"=from-file\n")
	t.Setenv(EnvFileVar, "")

	LoadEnvFile()

	if got, ok := os.LookupEnv(testVar); ok {
		t.Fatalf("%s = %q, want unset", testVar, got)
	}
}

// The environment is the explicit setting; the file is the convenience.
func TestLoadEnvFileDoesNotOverrideEnvironment(t *testing.T) {
	chdirWithEnvFile(t, EnvFile, testVar+"=from-file\n")
	t.Setenv(testVar, "from-environment")

	LoadEnvFile()

	if got := os.Getenv(testVar); got != "from-environment" {
		t.Fatalf("%s = %q, want %q", testVar, got, "from-environment")
	}
}

// A missing file is the common case and is not an error.
func TestLoadEnvFileMissingFile(t *testing.T) {
	t.Chdir(t.TempDir())

	LoadEnvFile()
}
