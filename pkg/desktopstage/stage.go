package desktopstage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pkg/serverstage"
)

// The staging root's layout, under Options.Root:
//
//	.lock                       held briefly: registering a run, and pruning
//	running/<digest>.<id>.lock  held by each CLI while its app runs
//	<digest>/                   an unpacked, verified app; complete or absent
//	<digest>.staging-*/         a download in flight, or one abandoned
//	<digest>.pruned-*/          a directory on its way out
//
// A digest directory is only ever created by renaming a complete staging into
// place, so one that exists holds every file the archive did.
const (
	rootLockName = ".lock"
	runningDir   = "running"
	// rootLockWait bounds waiting for another CLI's registration or prune,
	// which hold the root lock for directory listings and removals only, never
	// for a download.
	rootLockWait = 30 * time.Second
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Options configures a staging.
type Options struct {
	// Root is the directory holding one directory per digest. It must be this
	// user's alone: an existing digest directory is trusted as verified and
	// run without being hashed again, so whoever else can create one there
	// chooses what runs. A shared cache (~/.cache in a discobox is shared by
	// the pool) is not such a directory.
	Root string
	// Client fetches the archive. Nil is serverstage's default client, which
	// honors the environment's proxy settings.
	Client *http.Client
	// OnProgress, when set, is called while the archive downloads.
	OnProgress func(serverstage.Progress)
}

// App is a staged desktop app, held in use until Close.
type App struct {
	// Path is the executable to run.
	Path string
	// Dir is the digest directory the app was unpacked into.
	Dir string
	// Downloaded reports that this staging fetched the archive, rather than
	// finding it already unpacked.
	Downloaded bool

	lock     *filelock.Lock
	lockPath string
}

// Close marks the app no longer in use by this process, so a later staging
// may prune it. The process exiting does the same.
func (a *App) Close() error {
	if a == nil || a.lock == nil {
		return nil
	}
	_ = os.Remove(a.lockPath)
	err := a.lock.Release()
	a.lock = nil
	return err
}

// Stage returns m's app, downloading, verifying and unpacking it when it is not
// already staged, and prunes every other digest no running app uses.
//
// The returned App holds the digest in use until Close. It is held from before
// the root is looked at, so no other CLI's prune can remove the directory
// between this finding it and running what is in it.
func Stage(ctx context.Context, m Manifest, opts Options) (*App, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if opts.Root == "" {
		return nil, errors.New("stage desktop app: no staging directory")
	}
	if err := os.MkdirAll(filepath.Join(opts.Root, runningDir), 0o700); err != nil {
		return nil, fmt.Errorf("stage desktop app: %w", err)
	}
	digest := m.Asset.SHA256
	app, err := register(ctx, opts.Root, digest)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = app.Close()
		}
	}()
	app.Dir = filepath.Join(opts.Root, digest)
	app.Path = filepath.Join(app.Dir, filepath.FromSlash(m.Command))
	if !isDir(app.Dir) {
		if err := fetch(ctx, m, opts, app.Dir); err != nil {
			return nil, err
		}
		app.Downloaded = true
	}
	info, err := os.Stat(app.Path)
	if err != nil {
		return nil, fmt.Errorf("desktop app %s: %w", m.Version, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("desktop app %s: %s is a directory, not the app", m.Version, m.Command)
	}
	// Best effort: a root that cannot be tidied is not a reason to refuse to
	// run the app it holds.
	_ = Prune(ctx, opts.Root, digest)
	ok = true
	return app, nil
}

// register marks digest in use by this process: a lock file under running/,
// created and locked while the root lock is held, so a concurrent prune either
// sees it or finished before it existed.
func register(ctx context.Context, root, digest string) (*App, error) {
	rootLock, err := acquireRoot(ctx, root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rootLock.Release() }()
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(root, runningDir, digest+"."+strconv.Itoa(os.Getpid())+"-"+hex.EncodeToString(id)+".lock")
	lock, err := filelock.TryAcquire(lockPath)
	if err != nil {
		return nil, fmt.Errorf("stage desktop app: %w", err)
	}
	return &App{lock: lock, lockPath: lockPath}, nil
}

// acquireRoot takes the staging root's lock, waiting for another CLI holding it.
func acquireRoot(ctx context.Context, root string) (*filelock.Lock, error) {
	path := filepath.Join(root, rootLockName)
	deadline := time.Now().Add(rootLockWait)
	for {
		lock, err := filelock.TryAcquire(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return nil, fmt.Errorf("stage desktop app: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("stage desktop app: %s is held by another discobox", path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// fetch downloads and verifies the archive, unpacks it beside dir, and renames
// the result into place. A download that fails or does not verify leaves
// nothing at dir, so nothing unverified is ever run.
func fetch(ctx context.Context, m Manifest, opts Options, dir string) error {
	temp, err := os.MkdirTemp(opts.Root, m.Asset.SHA256+".staging-")
	if err != nil {
		return fmt.Errorf("stage desktop app: %w", err)
	}
	defer func() { _ = os.RemoveAll(temp) }()

	archive := filepath.Join(temp, m.Asset.Name)
	var report func(current, total int64)
	if opts.OnProgress != nil {
		report = func(current, total int64) {
			opts.OnProgress(serverstage.Progress{Asset: m.Asset.Name, Index: 1, Assets: 1, Current: current, Total: total})
		}
	}
	if err := serverstage.Download(ctx, opts.Client, m.Asset, archive, report); err != nil {
		return fmt.Errorf("download the desktop app: %w", err)
	}
	unpacked := filepath.Join(temp, "app")
	if err := unpack(archive, unpacked); err != nil {
		return fmt.Errorf("unpack the desktop app: %w", err)
	}
	if err := fsdurable.SyncDir(unpacked); err != nil {
		return err
	}
	if err := os.Rename(unpacked, dir); err != nil {
		// Another CLI staged the same digest first. It verified the same bytes,
		// so its directory is the one asked for.
		if isDir(dir) {
			return nil
		}
		return fmt.Errorf("stage desktop app into %s: %w", dir, err)
	}
	if err := fsdurable.SyncDir(opts.Root); err != nil {
		return err
	}
	if opts.OnProgress != nil {
		opts.OnProgress(serverstage.Progress{Assets: 1, Done: true})
	}
	return nil
}

// Prune removes every app under root, but keep, that no running app uses,
// and what interrupted stagings and prunes left behind for such a digest.
//
// A digest is in use while any lock under running/ for it is held. A lock file
// whose holder died is unlocked, and is removed here with what it guarded.
func Prune(ctx context.Context, root, keep string) error {
	rootLock, err := acquireRoot(ctx, root)
	if err != nil {
		return err
	}
	defer func() { _ = rootLock.Release() }()

	inUse := inUseDigests(filepath.Join(root, runningDir))
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		name := entry.Name()
		digest, _, _ := strings.Cut(name, ".")
		if !digestPattern.MatchString(digest) || digest == keep || inUse[digest] {
			continue
		}
		if name != digest && !strings.Contains(name, ".staging-") && !strings.Contains(name, ".pruned-") {
			continue
		}
		if err := removeAside(filepath.Join(root, name)); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// inUseDigests are the digests some live process holds a running lock for.
// Every lock file it can take is a dead holder's, and is removed.
func inUseDigests(dir string) map[string]bool {
	inUse := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return inUse
	}
	for _, entry := range entries {
		name := entry.Name()
		digest, _, _ := strings.Cut(name, ".")
		if !digestPattern.MatchString(digest) || !strings.HasSuffix(name, ".lock") {
			continue
		}
		path := filepath.Join(dir, name)
		lock, err := filelock.TryAcquire(path)
		if err != nil {
			// Held, or unreadable: either way it is not ours to call unused.
			inUse[digest] = true
			continue
		}
		_ = os.Remove(path)
		_ = lock.Release()
	}
	return inUse
}

// removeAside renames path to a .pruned- name before removing it, so a removal
// that stops part way (a file Windows will not let go of) leaves a name Prune
// recognizes and retries, never a digest directory missing some of its files.
func removeAside(path string) error {
	if strings.Contains(filepath.Base(path), ".pruned-") {
		return os.RemoveAll(path)
	}
	aside := path + ".pruned-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(path, aside); err != nil {
		return err
	}
	return os.RemoveAll(aside)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
