package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/sourcecache"
)

// Held answers which sandboxes the control plane still holds on this pool.
type Held func(context.Context) ([]string, error)

// SweepOptions are the windows Sweep keeps.
type SweepOptions struct {
	// NamespaceRetention is how long a namespace outlives the control plane
	// first saying its sandbox is gone. It covers a delivery that arrives
	// before its sandbox's row is listed, and an import, whose tree is
	// restored before its row (ADR 0123 §3).
	NamespaceRetention time.Duration
	// CacheRetention is how long a cache with no namespace is kept after its
	// last delivery before it is removed whole.
	CacheRetention time.Duration
}

// sweptFile, in the store's root, is touched when a sweep begins. Its age is
// how long ago that was, across restarts of the pool agent.
const sweptFile = ".swept"

// SweepIfDue runs Sweep when none has begun within interval, and records
// that one has. A store with no record — a new pool's, or one from before
// the record was kept — has it started now, so its first sweep is an interval
// away rather than at every start.
func (s *Store) SweepIfDue(ctx context.Context, interval time.Duration, held Held, opts SweepOptions, now time.Time, logger *slog.Logger) {
	stamp := filepath.Join(s.root, sweptFile)
	info, err := os.Stat(stamp)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, err := os.Stat(s.root); err != nil {
			// No cache has been made yet, so there is nothing to keep time for.
			return
		}
		if err := os.WriteFile(stamp, nil, 0o600); err != nil {
			logger.Warn("record the git cache sweep", "error", err)
		}
		return
	case err != nil:
		logger.Warn("read the git cache sweep record", "error", err)
		return
	case now.Sub(info.ModTime()) < interval:
		return
	}
	if err := os.Chtimes(stamp, now, now); err != nil {
		logger.Warn("record the git cache sweep", "error", err)
		return
	}
	s.Sweep(ctx, held, opts, now, logger)
}

// Sweep is the pool's periodic care of its caches: it removes the namespaces
// of sandboxes the control plane no longer holds, removes a cache nothing has
// used for its retention, and collects what is left.
//
// Each cache is collected with the cache to itself. Git's prune grace runs
// from an object's age, and a pin or a delta push creates a ref at objects
// that may be old, so a delivery and a prune must not overlap: Use holds a
// cache shared, and this holds it exclusively, one cache at a time.
func (s *Store) Sweep(ctx context.Context, held Held, opts SweepOptions, now time.Time, logger *slog.Logger) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("scan git caches", "root", s.root, "error", err)
		}
		return
	}
	ids, err := held(ctx)
	if err != nil {
		// Without the control plane's answer nothing is known to be gone, so
		// nothing is removed. Collecting is still safe: a namespace keeps
		// everything it reaches.
		logger.Warn("list the sandboxes the control plane holds; removing no git cache namespaces this pass", "error", err)
		ids = nil
	}
	heldKnown := err == nil
	heldSet := map[string]struct{}{}
	for _, sandboxID := range ids {
		heldSet[strings.TrimSpace(sandboxID)] = struct{}{}
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".git") {
			continue
		}
		path := filepath.Join(s.root, entry.Name())
		if _, err := os.Stat(filepath.Join(path, keyFile)); err != nil {
			continue
		}
		s.sweepCache(ctx, path, heldSet, heldKnown, opts, now, logger)
	}
}

func (s *Store) sweepCache(ctx context.Context, path string, held map[string]struct{}, heldKnown bool, opts SweepOptions, now time.Time, logger *slog.Logger) {
	lock := s.lock(path)
	lock.Lock()
	defer lock.Unlock()

	namespaces := 0
	markers, err := os.ReadDir(filepath.Join(path, namespacesDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warn("list git cache namespaces", "cache", path, "error", err)
		return
	}
	for _, marker := range markers {
		namespace := marker.Name()
		if strings.HasSuffix(namespace, unheldSuffix) {
			continue
		}
		namespaces++
		if !heldKnown {
			continue
		}
		file := filepath.Join(path, namespacesDir, namespace)
		owner, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		unheld := file + unheldSuffix
		if _, ok := held[strings.TrimSpace(string(owner))]; ok {
			_ = os.Remove(unheld)
			continue
		}
		since, ok := readStamp(unheld)
		if !ok {
			_ = os.WriteFile(unheld, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600)
			continue
		}
		if now.Sub(since) < opts.NamespaceRetention {
			continue
		}
		if err := removeNamespace(ctx, path, namespace); err != nil {
			logger.Warn("remove git cache namespace", "cache", path, "namespace", namespace, "error", err)
			continue
		}
		namespaces--
		logger.Info("removed the git cache namespace of a sandbox the control plane no longer holds", "cache", path, "namespace", namespace)
	}

	if namespaces == 0 && s.expired(path, opts, now) {
		if err := os.RemoveAll(path); err != nil {
			logger.Warn("remove unused git cache", "cache", path, "error", err)
			return
		}
		logger.Info("removed a git cache no sandbox uses", "cache", path)
		return
	}
	// Everything a ref does not reach goes now rather than after git's grace
	// period: nothing else can be writing this cache, so there is no
	// half-arrived push for the grace to protect.
	if _, err := git(ctx, path, nil, nil, "gc", "--quiet", "--prune=now"); err != nil {
		logger.Warn("collect git cache", "cache", path, "error", err)
	}
}

// expired reports whether a cache with no namespace has outlived its use. A
// cache made for one sandbox's source has no later user to keep it for.
func (s *Store) expired(path string, opts SweepOptions, now time.Time) bool {
	if key, err := os.ReadFile(filepath.Join(path, keyFile)); err == nil && sourcecache.ForOneSandbox(string(key)) {
		return true
	}
	used := filepath.Join(path, usedFile)
	info, err := os.Stat(used)
	if err != nil {
		if info, err = os.Stat(filepath.Join(path, keyFile)); err != nil {
			return false
		}
	}
	return now.Sub(info.ModTime()) >= opts.CacheRetention
}

func readStamp(path string) (time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	return at, err == nil
}

// removeNamespace deletes every ref of namespace and its record. The caller
// holds the cache exclusively.
func removeNamespace(ctx context.Context, path, namespace string) error {
	if !validNamespace(namespace) {
		return fmt.Errorf("%w: namespace %q", ErrInvalid, namespace)
	}
	prefix := "refs/namespaces/" + namespace + "/"
	// The symbolic ref first: deleting the branch it names would otherwise
	// leave it dangling, and it is not a ref for-each-ref lists once it is.
	_, _ = git(ctx, path, nil, nil, "symbolic-ref", "--delete", "--quiet", prefix+"HEAD")
	out, err := git(ctx, path, nil, nil, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		return err
	}
	var deletes bytes.Buffer
	for _, ref := range strings.Fields(out) {
		fmt.Fprintf(&deletes, "delete %s\n", ref)
	}
	if deletes.Len() > 0 {
		if _, err := git(ctx, path, deletes.Bytes(), nil, "update-ref", "--stdin"); err != nil {
			return err
		}
	}
	marker := filepath.Join(path, namespacesDir, namespace)
	_ = os.Remove(marker + unheldSuffix)
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
