package sshd

import (
	"context"
	"errors"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	idpkg "github.com/discobox-ai/x/id"
)

// errUsernameNotResolved is returned for any username that fails to resolve
// to exactly one sandbox, whether because nothing matched or because more
// than one candidate did. The two cases are deliberately not distinguished on
// the wire: an unauthenticated connection attempt must not learn which
// projects or sandboxes exist.
var errUsernameNotResolved = errors.New("sandbox not found")

// ResolveUsername turns an SSH username into a project and sandbox ID, using
// the same prefix-match rules the CLI uses (id.ResolveShort), per ADR 0024
// §1: `sbx_<id-or-prefix>` or `<sandbox>.<project>`.
//
// A username beginning with the sandbox ID prefix is always parsed as a bare
// sandbox ID/prefix, unconditionally — even if it happens to contain a `.` —
// since that prefix is reserved for this form. Anything else is split on the
// last `.` as `<sandbox>.<project>`.
func ResolveUsername(ctx context.Context, db *store.Store, username string) (projectID, sandboxID string, err error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", "", errUsernameNotResolved
	}
	if strings.HasPrefix(username, idpkg.PrefixSandbox+"_") {
		sandbox, err := db.FindSandboxByIDPrefix(ctx, username)
		if err != nil {
			return "", "", errUsernameNotResolved
		}
		return sandbox.ProjectID, sandbox.ID, nil
	}

	sandboxPart, projectPart, ok := cutLast(username, ".")
	if !ok || sandboxPart == "" || projectPart == "" {
		return "", "", errUsernameNotResolved
	}
	project, err := resolveProject(ctx, db, projectPart)
	if err != nil {
		return "", "", errUsernameNotResolved
	}
	sandbox, err := resolveSandboxInProject(ctx, db, project.ID, sandboxPart)
	if err != nil {
		return "", "", errUsernameNotResolved
	}
	return project.ID, sandbox.ID, nil
}

// cutLast splits s on the last occurrence of sep, unlike strings.Cut which
// splits on the first. A dotted username's project component is the
// authoritative suffix, so the sandbox component (before it) may itself
// contain dots without misparsing.
func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}

// resolveProject matches a project by exact name, then id.ResolveShort against
// project IDs — mirroring the CLI's resolvePoolID/resolveHarnessConfigID
// name-then-ID-prefix convention, since there is no GetProjectByName store
// method.
//
// A name is a key here rather than a label: it is unique per owner
// (idx_project_owner_name). This used to try a slug first, which no longer
// exists — a project is addressed by ID, and by name as a convenience.
func resolveProject(ctx context.Context, db *store.Store, value string) (*model.Project, error) {
	projects, err := db.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Name == value {
			return &p, nil
		}
	}
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	matches := idpkg.ResolveShort(value, ids)
	if len(matches) != 1 {
		return nil, errUsernameNotResolved
	}
	for _, p := range projects {
		if p.ID == matches[0] {
			return &p, nil
		}
	}
	return nil, errUsernameNotResolved
}

// resolveSandboxInProject matches a sandbox within one project by
// id.ResolveShort against sandbox IDs, matching cli/internal/cli/id.go's
// existing resolveSandboxID (ID/prefix only, not by display name).
func resolveSandboxInProject(ctx context.Context, db *store.Store, projectID, value string) (*model.Sandbox, error) {
	sandboxes, err := db.ListSandboxes(ctx, projectID, "", "")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(sandboxes))
	for _, sb := range sandboxes {
		ids = append(ids, sb.ID)
	}
	matches := idpkg.ResolveShort(value, ids)
	if len(matches) != 1 {
		return nil, errUsernameNotResolved
	}
	for _, sb := range sandboxes {
		if sb.ID == matches[0] {
			return &sb, nil
		}
	}
	return nil, errUsernameNotResolved
}
