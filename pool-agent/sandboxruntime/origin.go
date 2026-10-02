package sandboxruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// liveOriginsFileName records, per sandbox, where each clone-delivered
// source's live origin is: the developer's own Git directory, and the refs the
// source declares (ADR 0126 §4). It sits at the root of the sandbox's tree,
// beside the volumes rather than in one, because it is this pool's fact about
// what it can reach: the sandbox never sees it, and it does not travel with an
// export, since a path on this host means nothing to the pool a sandbox moves
// to — that pool's own create writes its own.
const liveOriginsFileName = "live-origins.json"

// liveOrigin is one source's entry in liveOriginsFileName.
type liveOrigin struct {
	// GitDirectory is the source's Git directory as the request named it, an
	// absolute path on the developer's host; the host mount prefix is applied
	// when it is opened, not when it is recorded.
	GitDirectory string `json:"gitDirectory"`
	// Refs are the refs the source declares, which a live origin advertises
	// besides HEAD and the branch HEAD names.
	Refs []string `json:"refs,omitempty"`
}

// writeLiveOrigins records every clone-delivered local source's live origin,
// replacing what an earlier create recorded. It runs on every create that
// builds volumes, so a repair or a re-pin leaves it describing the sources the
// sandbox has now; a sandbox with none has no file.
func (r *DockerSandboxRuntime) writeLiveOrigins(sandboxID string, sources []sandboxSource) error {
	origins := map[string]liveOrigin{}
	for _, source := range sources {
		if gitSourceAwaitsPush(source.git) {
			continue
		}
		local := strings.TrimSpace(optString(source.git.LocalDirectory))
		if local == "" {
			continue
		}
		gitDir, err := localGitDirectory(local)
		if err != nil {
			return fmt.Errorf("source %q: %w", source.slug, err)
		}
		origins[source.slug] = liveOrigin{GitDirectory: gitDir, Refs: sourceDeclaredRefs(source.git)}
	}
	path := filepath.Join(r.sandboxRoot(sandboxID), liveOriginsFileName)
	if len(origins) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(origins, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readLiveOrigin is slug's entry in the sandbox's live origins, if it has one.
func (r *DockerSandboxRuntime) readLiveOrigin(sandboxID, slug string) (liveOrigin, bool, error) {
	data, err := os.ReadFile(filepath.Join(r.sandboxRoot(sandboxID), liveOriginsFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return liveOrigin{}, false, nil
	}
	if err != nil {
		return liveOrigin{}, false, err
	}
	var origins map[string]liveOrigin
	if err := json.Unmarshal(data, &origins); err != nil {
		return liveOrigin{}, false, fmt.Errorf("decode %s: %w", liveOriginsFileName, err)
	}
	origin, ok := origins[slug]
	return origin, ok, nil
}

// sourceDeclaredRefs are the refs a source names for itself: the branch or tag
// it was checked out at, and the snapshot of a dirty workspace, whose parent
// is the commit the sandbox was spawned at. Advertising them is what keeps the
// sandbox's base fetchable when the developer switches branches or moves the
// one the sandbox came from (ADR 0126 §4).
func sourceDeclaredRefs(source workerapimodel.GitSource) []string {
	var refs []string
	if checkout, ok := source.Checkout.Get(); ok {
		name := strings.TrimSpace(optString(checkout.RefName))
		if name != "" && !strings.HasPrefix(name, "-") {
			switch strings.TrimSpace(optString(checkout.RefType)) {
			case "branch":
				refs = append(refs, "refs/heads/"+name)
			case "tag":
				refs = append(refs, "refs/tags/"+name)
			}
		}
	}
	if workspace, ok := source.Workspace.Get(); ok && workspace.Mode.Or(workerclient.GitSourceWorkspaceModeClean) == workerclient.GitSourceWorkspaceModeDirty {
		if ref := strings.TrimSpace(optString(workspace.SnapshotRef)); strings.HasPrefix(ref, "refs/") {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	return refs
}

// originLocation is the repository behind slug's origin route: the live
// origin when this pool can see the developer's Git directory, and otherwise
// the pool-side bare repository the client pushes into (ADR 0058). The sandbox
// is given the same address either way; which one answers is a fact about what
// this pool can reach, decided here rather than in the sandbox.
func (r *DockerSandboxRuntime) originLocation(sandboxID, slug string, env map[string]string) (GitRepositoryLocation, error) {
	origin, ok, err := r.readLiveOrigin(sandboxID, slug)
	if err != nil {
		return GitRepositoryLocation{}, err
	}
	if ok {
		location, visible, err := r.liveOriginLocation(origin)
		if err != nil {
			return GitRepositoryLocation{}, err
		}
		if visible {
			return location, nil
		}
	}
	repoPath := r.sandboxOriginPath(sandboxID, slug)
	if _, err := os.Stat(filepath.Join(repoPath, "HEAD")); err != nil {
		if os.IsNotExist(err) {
			return GitRepositoryLocation{}, fmt.Errorf("%w: %s", ErrRepositoryNotFound, slug)
		}
		return GitRepositoryLocation{}, err
	}
	uid, gid := sandboxUserFromEnv(env)
	return GitRepositoryLocation{Path: repoPath, UID: uid, GID: gid}, nil
}

// liveOriginLocation opens a live origin through this host's view of the
// developer's filesystem. It is visible only while it is still a real Git
// directory: a .git that has gone, or become a file or a symlink, is not the
// repository ADR 0093 lets a sandbox read, and the bare origin answers instead.
//
// The backend runs as the directory's owner, as every repository this pool
// serves is served — here that is the developer, which is also what keeps
// git's ownership check satisfied without a safe.directory exception.
func (r *DockerSandboxRuntime) liveOriginLocation(origin liveOrigin) (GitRepositoryLocation, bool, error) {
	gitDir := hostMountedLocalDirectory(origin.GitDirectory, r.hostMountPrefix)
	info, err := os.Lstat(gitDir)
	if errors.Is(err, fs.ErrNotExist) {
		return GitRepositoryLocation{}, false, nil
	}
	if err != nil {
		return GitRepositoryLocation{}, false, fmt.Errorf("stat live origin %s: %w", origin.GitDirectory, err)
	}
	if !info.IsDir() {
		return GitRepositoryLocation{}, false, nil
	}
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return GitRepositoryLocation{}, false, nil
		}
		return GitRepositoryLocation{}, false, fmt.Errorf("stat live origin %s: %w", origin.GitDirectory, err)
	}
	uid, gid := fileOwner(info)
	return GitRepositoryLocation{Path: gitDir, UID: uid, GID: gid, Live: true, Refs: origin.Refs}, true, nil
}
