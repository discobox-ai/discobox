// Package origin identifies the client host a sandbox is created from, and
// resolves the project directory a command acts on.
//
// Origin is provenance: it says which client a create request came from, never
// what to materialize. The server records it verbatim. Where on that client a
// sandbox belongs is its origin key — the host and where the primary source
// came from (internal/originkey) — so the origin carries no directory of its
// own.
//
// See docs/adr/0001-sandbox-origin-and-remote-source-push.md and
// docs/adr/0111-the-origin-is-the-client-and-its-key-names-where-the-source-came-from.md.
package origin

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/x/gitutil"
)

// Resolve returns the origin every create request carries: this client's host
// identity, with its hostname and user for display.
func Resolve() (apimodel.Origin, error) {
	host, err := hostid.Get()
	if err != nil {
		return apimodel.Origin{}, err
	}
	out := apimodel.Origin{HostId: host}
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
		out.Hostname = apiclientgen.NewOptString(hostname)
	}
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		out.User = apiclientgen.NewOptString(u.Username)
	}
	return out, nil
}

// ProjectPath returns the absolute project root for dir: its Git repository
// root, or dir itself when it is not in a repository. It is the directory a
// local source is cut from, and so the place a directory's origin key hashes.
func ProjectPath(ctx context.Context, dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve directory %s: %w", dir, err)
	}
	root, err := gitutil.Root(ctx, abs)
	if err != nil {
		// Not a repository. The directory itself is then the project, so
		// listing and creating still work here rather than failing; the error
		// says only that there is no repo root to prefer.
		return abs, nil //nolint:nilerr // absence of a repository is a fallback, not a failure
	}
	return root, nil
}
