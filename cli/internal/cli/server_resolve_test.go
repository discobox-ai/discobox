package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/serverstage"
)

// fakeInstall is a directory holding a discobox with a server beside it, which
// is what `task build` produces and what a package that ships both installs.
func fakeInstall(t *testing.T, withServer bool) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "discobox"), "the cli")
	if withServer {
		write(t, filepath.Join(dir, serverBinaryName()), "the server")
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func servedManifest(t *testing.T, version string) (serverstage.Manifest, *int) {
	t.Helper()
	body := []byte("the downloaded server")
	fetches := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*fetches++
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	sum := sha256.Sum256(body)
	return serverstage.Manifest{
		Version: version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Command: serverBinaryName(),
		Assets: []serverstage.Asset{{
			Name:       serverBinaryName(),
			URL:        server.URL + "/discobox-server",
			SHA256:     hex.EncodeToString(sum[:]),
			Size:       int64(len(body)),
			Executable: true,
		}},
	}, fetches
}

func manifestFile(t *testing.T, manifest serverstage.Manifest) string {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A server binary next to the CLI is used as it is. It is what keeps a
// development build — which carries no manifest — working with nothing
// configured, and what lets a package that ships both skip a download.
func TestResolveUsesAServerBesideTheExecutable(t *testing.T) {
	dir := fakeInstall(t, true)
	resolver := serverResolver{executable: filepath.Join(dir, "discobox"), stageRoot: t.TempDir()}

	got, err := resolver.resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, serverBinaryName()); got != want {
		t.Fatalf("resolved %q, want the sibling %q", got, want)
	}
}

// PATH is deliberately not searched: a directory mate of the executable is no
// more attacker-controlled than the executable itself, and a PATH entry is a
// completely different claim.
func TestResolveDoesNotSearchPath(t *testing.T) {
	elsewhere := t.TempDir()
	write(t, filepath.Join(elsewhere, serverBinaryName()), "not this one")
	t.Setenv("PATH", elsewhere+string(os.PathListSeparator)+os.Getenv("PATH"))

	resolver := serverResolver{executable: filepath.Join(fakeInstall(t, false), "discobox"), stageRoot: t.TempDir()}
	_, err := resolver.resolve(context.Background())
	if err == nil {
		t.Fatal("a server on PATH was resolved")
	}
}

// A build with nothing to download and nothing beside it says so, and says both
// ways out of it. It does not guess at a URL.
func TestResolveSaysWhatIsMissing(t *testing.T) {
	resolver := serverResolver{executable: filepath.Join(fakeInstall(t, false), "discobox"), stageRoot: t.TempDir()}
	_, err := resolver.resolve(context.Background())
	if err == nil {
		t.Fatal("a build with no server resolved one")
	}
	for _, want := range []string{"no server download", serverBinaryName(), ServerBinaryEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// An explicit binary outranks everything, because somebody named it.
func TestResolvePrefersTheNamedBinary(t *testing.T) {
	dir := fakeInstall(t, true)
	named := filepath.Join(t.TempDir(), "my-server")
	write(t, named, "a build of my own")

	resolver := serverResolver{
		source:     serverSource{binary: named},
		executable: filepath.Join(dir, "discobox"),
		stageRoot:  t.TempDir(),
	}
	got, err := resolver.resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != named {
		t.Fatalf("resolved %q, want the named binary %q", got, named)
	}
}

func TestResolveReportsANamedBinaryThatIsNotThere(t *testing.T) {
	resolver := serverResolver{
		source:    serverSource{binary: filepath.Join(t.TempDir(), "nowhere")},
		stageRoot: t.TempDir(),
	}
	if _, err := resolver.resolve(context.Background()); err == nil {
		t.Fatal("a binary that is not there resolved")
	}
}

// An explicit manifest is an instruction too, so it outranks whatever happens
// to be lying beside the binary.
func TestResolveStagesWhenAManifestIsNamed(t *testing.T) {
	manifest, fetches := servedManifest(t, "v9.9.9")
	dir := fakeInstall(t, true)
	root := t.TempDir()
	resolver := serverResolver{
		source:     serverSource{manifest: manifestFile(t, manifest)},
		executable: filepath.Join(dir, "discobox"),
		stageRoot:  root,
	}

	got, err := resolver.resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "v9.9.9", serverBinaryName()); got != want {
		t.Fatalf("resolved %q, want the staged %q", got, want)
	}
	if *fetches != 1 {
		t.Fatalf("the named manifest was fetched %d times, want 1", *fetches)
	}
	// And a second resolve costs nothing.
	if _, err := resolver.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *fetches != 1 {
		t.Fatalf("a staged server was downloaded again (%d fetches)", *fetches)
	}
}

// Staging a server for another machine is fine; running one is not.
func TestResolveRefusesAnotherPlatformsServer(t *testing.T) {
	manifest, _ := servedManifest(t, "v9.9.9")
	manifest.Arch = "somethingelse"
	resolver := serverResolver{
		source:     serverSource{manifest: manifestFile(t, manifest)},
		executable: filepath.Join(fakeInstall(t, false), "discobox"),
		stageRoot:  t.TempDir(),
	}
	_, err := resolver.resolve(context.Background())
	if err == nil {
		t.Fatal("a server for another platform resolved")
	}
	if !strings.Contains(err.Error(), "somethingelse") {
		t.Fatalf("error %q does not say which platform it is for", err)
	}
}

// The line a download narrates on is the same one the launch it is part of
// uses, so it has to render every shape a report comes in.
func TestServerStageText(t *testing.T) {
	tests := []struct {
		report serverstage.Progress
		want   string
	}{
		// The one-asset case, which is every release so far: no count, no file
		// name, and bytesSuffix's tail — the same sentence image staging draws
		// on this line minutes later.
		{serverstage.Progress{Asset: "discobox-server", Index: 1, Assets: 1, Total: 1024, Current: 512},
			"Downloading server — 512 B of 1.0 KiB"},
		{serverstage.Progress{Asset: "relay.bin", Index: 2, Assets: 2, Current: 2048},
			"Downloading server (2 of 2): relay.bin — 2.0 KiB"},
		// A server that has declared no length yet still says what it is doing.
		{serverstage.Progress{Asset: "discobox-server", Index: 1, Assets: 1},
			"Downloading server"},
		{serverstage.Progress{Done: true}, "Server downloaded"},
	}
	for _, test := range tests {
		if got := serverStageText(test.report); got != test.want {
			t.Fatalf("serverStageText(%+v) = %q, want %q", test.report, got, test.want)
		}
	}
}

// The flag has to survive the root command's own hook. It did not: the hook
// read DISCOBOX_SERVER_BINARY into the same field after cobra had parsed the
// flag into it, so --binary was silently discarded on every invocation.
func TestServerBinaryFlagAndEnvReachResolution(t *testing.T) {
	named := filepath.Join(t.TempDir(), "not-there")
	for _, from := range []string{"flag", "environment"} {
		t.Run(from, func(t *testing.T) {
			root, _ := newRootCommand()
			args := []string{"admin", "server"}
			if from == "flag" {
				args = append(args, "--binary", named)
			} else {
				t.Setenv(ServerBinaryEnv, named)
			}
			root.SetArgs(args)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)

			err := root.Execute()
			if err == nil {
				t.Fatal("running a server binary that is not there succeeded")
			}
			if !strings.Contains(err.Error(), named) {
				t.Fatalf("error %q does not name the binary from the %s", err, from)
			}
		})
	}
}
