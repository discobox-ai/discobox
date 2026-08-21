// Package origin identifies the client host and project directory a sandbox is
// created from.
//
// Origin is provenance: it says where a create request came from, never what to
// materialize. The server records it verbatim and uses it to answer "which
// sandboxes did I start from this directory?" — a question the source alone
// cannot answer once the server is remote, because a local path is meaningless
// on another machine and collides across hosts and users.
//
// See docs/adr/0001-sandbox-origin-and-remote-source-push.md.
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
	"github.com/discobox-ai/discobox/internal/gitutil"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/discobox/internal/originkey"
)

// Resolve returns the origin for a create or list request made from dir.
//
// dir's Git repository root is the project path, matching how a project is
// commonly understood as the repo you started from. Outside a repository the
// directory itself is the project, so listing still works there rather than
// failing.
func Resolve(ctx context.Context, dir string) (apimodel.Origin, error) {
	host, err := hostid.Get()
	if err != nil {
		return apimodel.Origin{}, err
	}
	projectPath, err := ProjectPath(ctx, dir)
	if err != nil {
		return apimodel.Origin{}, err
	}
	out := apimodel.Origin{
		HostId:      host,
		ProjectPath: projectPath,
	}
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
		out.Hostname = apiclientgen.NewOptString(hostname)
	}
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		out.User = apiclientgen.NewOptString(u.Username)
	}
	return out, nil
}

// ProjectPath returns the absolute project root for dir: its Git repository
// root, or dir itself when it is not in a repository.
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

// Key is the indexed identity the server stores and filters on.
func Key(o apimodel.Origin) string {
	return originkey.Of(o.HostId, o.ProjectPath)
}
