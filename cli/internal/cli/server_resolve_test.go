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
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/imagecache"
	"github.com/discobox-ai/discobox/imagecache/imagecachetest"
	"github.com/discobox-ai/discobox/serverstage"
	"github.com/discobox-ai/discobox/version"
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
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
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
			URLs:       []string{server.URL + "/discobox-server"},
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
	if want := filepath.Join(root, runtime.GOOS+"-"+runtime.GOARCH, "v9.9.9", serverBinaryName()); got != want {
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

// The line an image download narrates on is the one the server's download used
// a moment earlier, in the words a pool's staging uses later.
func TestImagesStageText(t *testing.T) {
	tests := []struct {
		report imagecache.Progress
		want   string
	}{
		{imagecache.Progress{Image: "ghcr.io/discobox-ai/discobox-harness-codex:v1", Index: 2, Images: 5, Total: 1024, Current: 512},
			"Downloading images (2 of 5): discobox-harness-codex:v1 — 512 B of 1.0 KiB"},
		// Before its manifests are read, an image has no total to count toward.
		{imagecache.Progress{Image: "ghcr.io/x/a:v1", Index: 1, Images: 1}, "Downloading images (1 of 1): a:v1"},
		{imagecache.Progress{Images: 5, Done: true}, "Images downloaded"},
	}
	for _, test := range tests {
		if got := imagesStageText(test.report); got != test.want {
			t.Fatalf("imagesStageText(%+v) = %q, want %q", test.report, got, test.want)
		}
	}
}

// setVersion makes this binary report version, as a release build's linker
// would.
func setVersion(t *testing.T, v string) {
	t.Helper()
	previous := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = previous })
}

// The images come from the server this CLI staged, asked before it is started,
// and land once in the image cache a pool loads them from: staging again
// downloads nothing.
func TestStageImagesStagesWhatTheStagedServerNames(t *testing.T) {
	setVersion(t, "v9.9.9")
	registry := imagecachetest.NewRegistry(t)
	image := registry.Publish("x/agent", "v1", []byte("base"))
	manifest, _ := servedManifest(t, "v9.9.9")
	var asked []string
	resolver := serverResolver{
		source:    serverSource{manifest: manifestFile(t, manifest)},
		stageRoot: t.TempDir(),
		imageRoot: filepath.Join(t.TempDir(), "images"),
		client:    registry.Client(),
		serverImages: func(_ context.Context, server string, _ []string) ([]string, error) {
			asked = append(asked, server)
			return []string{image.Reference}, nil
		},
	}
	dir, err := resolver.stage(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(dir, manifest.Command)
	staged, err := resolver.stageImages(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{server}) {
		t.Fatalf("asked %v, want the staged server %s", asked, server)
	}
	if len(staged) != 1 || staged[0].Digest != image.Index {
		t.Fatalf("staged %+v, want %s at %s", staged, image.Reference, image.Index)
	}
	if _, err := imagecache.Open(resolver.imageRoot).Lookup(image.Reference, imagecache.PoolPlatform()); err != nil {
		t.Fatal(err)
	}
	fetched := registry.Fetches()
	if _, err := resolver.stageImages(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	if registry.Fetches() != fetched {
		t.Fatal("staging the same images again downloaded them again")
	}
}

// Only a server of this CLI's own release is asked. An older one takes the
// question as nothing and starts serving, so a binary named with --binary, one
// staged from another version's manifest, and a build with no manifest at all
// are never asked.
func TestStageImagesAsksOnlyAServerOfThisRelease(t *testing.T) {
	setVersion(t, "v9.9.9")
	asked := 0
	ask := func(context.Context, string, []string) ([]string, error) {
		asked++
		return nil, nil
	}
	current, _ := servedManifest(t, "v9.9.9")
	older, _ := servedManifest(t, "v9.9.8")
	stagedFrom := func(manifest serverstage.Manifest) (serverResolver, string) {
		resolver := serverResolver{
			source:       serverSource{manifest: manifestFile(t, manifest)},
			stageRoot:    t.TempDir(),
			imageRoot:    t.TempDir(),
			serverImages: ask,
		}
		dir, err := resolver.stage(context.Background(), manifest)
		if err != nil {
			t.Fatal(err)
		}
		return resolver, filepath.Join(dir, manifest.Command)
	}
	ofThisRelease, server := stagedFrom(current)
	ofAnother, olderServer := stagedFrom(older)
	for name, check := range map[string]func() ([]imagecache.Staged, error){
		"a named binary": func() ([]imagecache.Staged, error) {
			return ofThisRelease.stageImages(context.Background(), "/opt/discobox-server")
		},
		"another release": func() ([]imagecache.Staged, error) { return ofAnother.stageImages(context.Background(), olderServer) },
		"a build with no manifest": func() ([]imagecache.Staged, error) {
			return serverResolver{imageRoot: t.TempDir(), serverImages: ask}.stageImages(context.Background(), server)
		},
	} {
		if staged, err := check(); err != nil || len(staged) != 0 {
			t.Fatalf("%s: stageImages() = %+v, %v; want nothing", name, staged, err)
		}
	}
	if asked != 0 {
		t.Fatalf("asked %d servers that are not of this release", asked)
	}
	if _, err := ofThisRelease.stageImages(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Fatal("the staged server of this release was not asked")
	}
}

// The server an autolaunch starts is told where its images were staged, or it
// would pull every one of them again.
func TestLocalServerEnvNamesTheImageCache(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv(ImageCacheEnv, "")
	want := ImageCacheEnv + "=" + filepath.Join(state, "discobox", "images")
	if env := localServerEnv("unix:///tmp/discobox.sock"); !slices.Contains(env, want) {
		t.Fatalf("env = %v, want %s", env, want)
	}
}
