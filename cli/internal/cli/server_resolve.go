package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/discobox-ai/discobox/serverstage"
)

// The server is a separate program (ADR 0099). The CLI does not contain it; it
// resolves one, staging the assets this build was cut against when there is
// nothing on the machine to run.

const (
	// ServerBinaryEnv names a server binary to run as-is, skipping everything
	// below. It is the escape hatch for running a build of the server that no
	// manifest describes.
	ServerBinaryEnv = "DISCOBOX_SERVER_BINARY"
	// ServerManifestEnv names a manifest file or URL to stage from instead of
	// the one this build carries.
	ServerManifestEnv = "DISCOBOX_SERVER_MANIFEST"
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
	client    *http.Client
	// onProgress, when set, is called while assets are downloaded.
	onProgress func(serverstage.Progress)
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
		return serverstage.Load(ctx, named, r.client)
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
