package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/imagecache"
	"github.com/discobox-ai/discobox/serverstage"
	"github.com/discobox-ai/discobox/version"
)

// The server is a separate program (ADR 0099). The CLI does not contain it; it
// resolves one, staging the assets this build was cut against when there is
// nothing on the machine to run.

const (
	// ServerBinaryEnv names a server binary to run as-is, skipping everything
	// below. It is the escape hatch for running a build of the server that no
	// manifest describes.
	ServerBinaryEnv = "DISCOBOX_SERVER_BINARY"
	// ServerManifestEnv names a manifest file to stage from instead of the one
	// this build carries.
	ServerManifestEnv = "DISCOBOX_SERVER_MANIFEST"
	// ImageCacheEnv names the image cache the server's images are staged into
	// and a server's pools load them from (ADR 0113). It is the server's own
	// setting, so naming it here names it for both.
	ImageCacheEnv = "DISCOBOX_IMAGE_CACHE_DIR"
)

// serverSource is where the server binary is to come from: what the flags and
// the environment say, before resolution consults the machine.
type serverSource struct {
	// binary is --binary, or ServerBinaryEnv.
	binary string
	// manifest is --manifest, or ServerManifestEnv.
	manifest string
	// force restages even when this version is already on disk.
	force bool
}

// serverResolver answers "which program is the server here".
//
// It holds what it consults rather than reading it from the process, so a test
// can put a binary beside a fake executable and a staging root in a temporary
// directory and get the same answers a user would.
type serverResolver struct {
	source serverSource
	// executable is the running discobox. Its directory is searched, and
	// nothing else is: see resolve.
	executable string
	// stageRoot holds one directory per staged server version.
	stageRoot string
	// imageRoot is the image cache the server's images are staged into.
	imageRoot string
	client    *http.Client
	// onProgress, when set, is called while assets are downloaded.
	onProgress func(serverstage.Progress)
	// onImageProgress, when set, is called while images are downloaded.
	onImageProgress func(imagecache.Progress)
	// env is what the server this resolver stages will be started with, so
	// asking it which images it runs is answered by the configuration it will
	// actually read.
	env []string
	// serverImages asks a server which images it runs. Nil runs the server
	// and asks it.
	serverImages func(ctx context.Context, server string, env []string) ([]string, error)
}

func (a *App) serverResolver(onProgress func(serverstage.Progress)) serverResolver {
	// The environment is read here rather than as a flag default, because the
	// flags belong to `admin server` and the autolaunch has none — and a flag
	// whose default is an environment variable is one that a hook running later
	// can silently overwrite, which is exactly how --binary was ignored.
	source := a.serverSource
	if source.binary == "" {
		source.binary = strings.TrimSpace(os.Getenv(ServerBinaryEnv))
	}
	if source.manifest == "" {
		source.manifest = strings.TrimSpace(os.Getenv(ServerManifestEnv))
	}
	executable, err := os.Executable()
	if err == nil {
		// Through the symlink: a package manager puts the command on PATH as a
		// link into its own directory, and the server it installed is beside
		// the file rather than beside the link.
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
	}
	return serverResolver{
		source:     source,
		executable: executable,
		stageRoot:  stagedServerRoot(),
		imageRoot:  stagedImagesRoot(),
		env:        localServerEnv(a.serverURL),
		onProgress: onProgress,
	}
}

// resolve returns the path of the server binary to run, staging it when that is
// what it takes.
//
// The order is: what the caller named, then what is installed beside this
// binary, then what this build was cut against.
//
// The sibling is what keeps a build with no manifest working with nothing
// configured — `task build` writes both binaries into build/ — and what lets a
// package that ships the two together avoid a download it has no use for.
// PATH is deliberately not searched: a directory mate of the executable is no
// more attacker-controlled than the executable itself, and "the server was
// replaced by something earlier in PATH" is not a failure mode worth buying a
// convenience with.
func (r serverResolver) resolve(ctx context.Context) (string, error) {
	if named := r.source.binary; named != "" {
		// Absolute, because a bare name means two different files to the two
		// halves of this: os.Stat resolves it against the working directory
		// and exec.Command hands it to PATH. So `--binary discobox-server` in
		// build/ would stat the file in front of it and then run whichever
		// one PATH found — or fail saying there was none, with the file it had
		// just checked sitting right there. PATH is not searched for a server
		// (ADR 0099 §6), and "used as-is" has to mean the file the caller
		// named.
		named, err := filepath.Abs(named)
		if err != nil {
			return "", fmt.Errorf("server binary %s: %w", r.source.binary, err)
		}
		if _, err := os.Stat(named); err != nil {
			return "", fmt.Errorf("server binary %s: %w", named, err)
		}
		return named, nil
	}
	// An explicit manifest is an instruction, so it outranks whatever happens
	// to be lying beside the binary.
	if r.source.manifest == "" {
		if sibling, ok := r.sibling(); ok {
			return sibling, nil
		}
	}
	manifest, err := r.manifest(ctx)
	if errors.Is(err, serverstage.ErrNoManifest) {
		// The end of the order, and the only place that can say what the whole
		// of it was: this build has nothing to download and the machine has
		// nothing to run, so name both ways out.
		return "", fmt.Errorf(
			"%w, and there is no %s beside %s\nBuild one with `go tool task build`, or name one with --binary or %s",
			err, serverBinaryName(), r.executableName(), ServerBinaryEnv)
	}
	if err != nil {
		return "", err
	}
	if !manifest.ForThisPlatform() {
		return "", fmt.Errorf("that manifest describes a server for %s, and this is %s/%s",
			manifest.Platform(), runtime.GOOS, runtime.GOARCH)
	}
	dir, err := r.stage(ctx, manifest)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, manifest.Command), nil
}

// sibling is a server binary installed next to this one.
func (r serverResolver) sibling() (string, bool) {
	if r.executable == "" {
		return "", false
	}
	path := filepath.Join(filepath.Dir(r.executable), serverBinaryName())
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}

// serverBinaryName is what the server is called on this platform.
func serverBinaryName() string {
	if runtime.GOOS == "windows" {
		return "discobox-server.exe"
	}
	return "discobox-server"
}

// manifest is the description of the server to stage: the one the caller named,
// or the one this build carries.
func (r serverResolver) manifest(ctx context.Context) (serverstage.Manifest, error) {
	if named := r.source.manifest; named != "" {
		return serverstage.Load(ctx, named)
	}
	return serverstage.Default()
}

// stage downloads and verifies the manifest's assets.
func (r serverResolver) stage(ctx context.Context, manifest serverstage.Manifest) (string, error) {
	// The staging root is created and locked down here rather than by the
	// staging itself: on Windows a directory's permissions are inherited from
	// its parent, so the tree is only this user's if something at the top of it
	// says so, and what is staged under it is a file that gets executed.
	if err := ensureStateDir(r.stageRoot); err != nil {
		return "", err
	}
	return serverstage.Stage(ctx, manifest, serverstage.Options{
		Root:       r.stageRoot,
		Client:     r.client,
		Force:      r.source.force,
		OnProgress: r.onProgress,
	})
}

// stageImages asks the server about to be started which images a first run on
// this machine will want, and stages them into the image cache its pools load
// them from (ADR 0113).
//
// Only a server staged from the manifest this CLI was linked with is asked. It
// is the same release as this CLI, so it answers the question; an older server
// — named with --binary, installed beside this one, or staged from another
// version's --manifest — would take the argument as nothing and start serving.
func (r serverResolver) stageImages(ctx context.Context, server string) ([]imagecache.Staged, error) {
	manifest, err := r.manifest(ctx)
	if errors.Is(err, serverstage.ErrNoManifest) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if manifest.Version != version.String() || !manifest.ForThisPlatform() {
		return nil, nil
	}
	if dir, ok := serverstage.Staged(r.stageRoot, manifest); !ok || filepath.Join(dir, manifest.Command) != server {
		return nil, nil
	}
	ask := r.serverImages
	if ask == nil {
		ask = askServerImages
	}
	images, err := ask(ctx, server, r.env)
	if err != nil {
		return nil, fmt.Errorf("ask the server which images it runs: %w", err)
	}
	if len(images) == 0 {
		return nil, nil
	}
	// Locked down for the reason the staging root is: on Windows a directory
	// is only this user's if something at the top of it says so.
	if err := ensureStateDir(r.imageRoot); err != nil {
		return nil, err
	}
	return imagecache.Open(r.imageRoot).Stage(ctx, images, imagecache.Options{
		Client:     r.client,
		OnProgress: r.onImageProgress,
	})
}

// serverImagesTimeout bounds asking a server which images it runs. It is the
// binary about to be started, and not yet one that has shown it answers.
const serverImagesTimeout = 30 * time.Second

// askServerImages runs `<server> images`, which prints one reference per line.
//
// In the environment the server itself will be started with, not merely this
// process's: the answer is read out of the server's configuration (ADR 0113
// §1), and a variable the launch adds — or one it does not carry — would
// otherwise name an image the server will never run.
func askServerImages(ctx context.Context, server string, env []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, serverImagesTimeout)
	defer cancel()
	//nolint:gosec // The path is the server this CLI staged and verified (ADR 0099).
	cmd := exec.CommandContext(ctx, server, "images")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, err
	}
	var images []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			images = append(images, line)
		}
	}
	return images, nil
}

// executableName is this binary, for an error to name. "discobox" when there is
// nothing better, which is what it is called.
func (r serverResolver) executableName() string {
	if r.executable == "" {
		return "discobox"
	}
	return r.executable
}

// resolveServer is the App's own resolution, which is what both `discobox admin
// server` and the autolaunch go through.
func (a *App) resolveServer(ctx context.Context, onProgress func(serverstage.Progress)) (string, error) {
	return a.serverResolver(onProgress).resolve(ctx)
}
