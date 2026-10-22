package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sandboxuser"
	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/gitcache"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// sourceCachesFileName records, per sandbox, which of this pool's Git caches
// each cache-delivered source's namespace is in (ADR 26-10-10-545 §2). It
// sits at the root of the sandbox's tree, beside the volumes rather than in
// one, because it is this pool's fact about where it keeps something: the
// sandbox never sees it, and a create writes it from the request, so it does
// not need to travel with an export.
const sourceCachesFileName = "source-caches.json"

// legacyLiveOriginsFileName is what a pool wrote when it served a
// clone-delivered local source live from the developer's Git directory (ADR
// 0126 §4). Nothing reads it now; a create removes it.
const legacyLiveOriginsFileName = "live-origins.json"

// gitSourceCacheKey is the cache a source is delivered into, empty for a
// source that is not cache-delivered.
func gitSourceCacheKey(source workerapimodel.GitSource) string {
	if delivery, ok := source.Delivery.Get(); !ok || delivery != workerclient.GitSourceDeliveryCache {
		return ""
	}
	return strings.TrimSpace(optString(source.CacheKey))
}

// writeSourceCaches records each cache-delivered source's cache, replacing
// what an earlier create recorded. It runs on every create that builds
// volumes, so it describes the sources the sandbox has now; a sandbox with
// none has no file.
func (r *DockerSandboxRuntime) writeSourceCaches(sandboxID string, sources []sandboxSource) error {
	root := r.sandboxRoot(sandboxID)
	if err := os.Remove(filepath.Join(root, legacyLiveOriginsFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	caches := map[string]string{}
	for _, source := range sources {
		if key := gitSourceCacheKey(source.git); key != "" {
			caches[source.slug] = key
		}
	}
	path := filepath.Join(root, sourceCachesFileName)
	if len(caches) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(caches, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readSourceCaches is the sandbox's cache-delivered sources and the cache
// each is in, by slug. None is not an error.
func (r *DockerSandboxRuntime) readSourceCaches(sandboxID string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(r.sandboxRoot(sandboxID), sourceCachesFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var caches map[string]string
	if err := json.Unmarshal(data, &caches); err != nil {
		return nil, fmt.Errorf("decode %s: %w", sourceCachesFileName, err)
	}
	return caches, nil
}

// originLocation is the repository behind slug's origin route, as the sandbox
// itself reaches it: its namespace of this pool's cache, served fetch-only,
// for a cache-delivered source (ADR 26-10-10-545 §7), and otherwise the bare
// repository an earlier pool made for the sandbox (ADR 0058), which a client
// pushes into. The sandbox is given the same address either way.
func (r *DockerSandboxRuntime) originLocation(ctx context.Context, sandboxID, slug string, env map[string]string) (GitRepositoryLocation, error) {
	caches, err := r.readSourceCaches(sandboxID)
	if err != nil {
		return GitRepositoryLocation{}, err
	}
	if key := caches[slug]; key != "" {
		path, release, err := r.gitCache.Use(ctx, key)
		if err != nil {
			return GitRepositoryLocation{}, err
		}
		return GitRepositoryLocation{
			Path: path, UID: -1, GID: -1,
			Namespace: gitcache.Namespace(sandboxID, slug),
			FetchOnly: true,
			Release:   release,
		}, nil
	}
	repoPath := r.sandboxOriginPath(sandboxID, slug)
	uid, gid := sandboxUserFromEnv(env)
	if _, err := os.Stat(filepath.Join(repoPath, "HEAD")); err != nil {
		if !os.IsNotExist(err) {
			return GitRepositoryLocation{}, err
		}
		served, err := r.poolServesSlug(sandboxID, slug)
		if err != nil {
			return GitRepositoryLocation{}, err
		}
		if !served {
			return GitRepositoryLocation{}, fmt.Errorf("%w: %s", ErrRepositoryNotFound, slug)
		}
		// A source this pool once served live from the developer's Git
		// directory, in a sandbox that has not been rebuilt since: it gets
		// its repository now, empty until a client next pushes (ADR
		// 26-10-10-545 §8).
		ownerUID, ownerGID := int64(uid), int64(gid)
		owner := sandboxuser.User{UID: &ownerUID, GID: &ownerGID}
		if err := r.initGitOrigin(ctx, repoPath, "", owner); err != nil {
			return GitRepositoryLocation{}, fmt.Errorf("prepare source origin %q: %w", slug, err)
		}
	}
	return GitRepositoryLocation{Path: repoPath, UID: uid, GID: gid}, nil
}

// poolServesSlug reports whether the sandbox's runtime-config document names
// slug as a source whose origin is this pool's route: the pool's record of
// what it told the sandbox to fetch from, and so of what it has to serve.
func (r *DockerSandboxRuntime) poolServesSlug(sandboxID, slug string) (bool, error) {
	doc, ok, err := r.readRuntimeConfig(sandboxID)
	if err != nil || !ok {
		return false, err
	}
	for _, source := range doc.Sources {
		if source.Slug == slug {
			return source.OriginURL == proxyagent.OriginURL(r.projectID, r.poolID, sandboxID, slug), nil
		}
	}
	return false, nil
}

// adoptImportedOrigins reads into this pool's cache the repositories an
// import restored for the sandbox's cache-delivered sources — an export
// carries each source's namespace as a repository of its own (ADR
// 26-10-10-545 §9) — and removes them from the tree: the namespace is the
// origin from here on. A namespace that already holds a branch is left as it
// is, so what an interrupted export left behind is never read back over
// something newer.
func (r *DockerSandboxRuntime) adoptImportedOrigins(ctx context.Context, sandboxID string, sources []sandboxSource) error {
	for _, source := range sources {
		key := gitSourceCacheKey(source.git)
		if key == "" {
			continue
		}
		imported := r.sandboxOriginPath(sandboxID, source.slug)
		if _, err := os.Stat(filepath.Join(imported, "HEAD")); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		path, release, err := r.gitCache.Use(ctx, key)
		if err != nil {
			return err
		}
		delivered, err := r.gitCache.HasBranches(ctx, path, sandboxID, source.slug)
		if err == nil && !delivered {
			err = r.gitCache.Import(ctx, path, sandboxID, source.slug, imported)
		}
		release()
		if err != nil {
			return fmt.Errorf("source %q: %w", source.slug, err)
		}
		if err := os.RemoveAll(imported); err != nil {
			return err
		}
	}
	return nil
}

// exportCachedOrigins writes each of the sandbox's cache-delivered sources'
// namespaces out as a repository under the tree's origins directory, where an
// export reads a sandbox's origins from, and returns what removes them again.
func (r *DockerSandboxRuntime) exportCachedOrigins(ctx context.Context, sandboxID string) (func(), error) {
	caches, err := r.readSourceCaches(sandboxID)
	if err != nil {
		return nil, err
	}
	var written []string
	cleanup := func() {
		for _, path := range written {
			_ = os.RemoveAll(path)
		}
	}
	for slug, key := range caches {
		path, release, err := r.gitCache.Use(ctx, key)
		if err != nil {
			cleanup()
			return nil, err
		}
		dest := r.sandboxOriginPath(sandboxID, slug)
		written = append(written, dest)
		err = r.gitCache.Export(ctx, path, sandboxID, slug, dest)
		release()
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("source %q: %w", slug, err)
		}
	}
	return cleanup, nil
}
