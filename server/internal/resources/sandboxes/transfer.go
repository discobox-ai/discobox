package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandboxmeta"
	"github.com/discobox-ai/discobox/secretformat"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/sandboxexport"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/tarsums"
	"github.com/discobox-ai/discobox/version"
	"github.com/discobox-ai/x/id"
)

// Export and import are the two halves of moving a discobox between servers
// (ADR 0123). Between them they have one rule that everything else follows
// from: what travels is what a container rebuild already preserves, so an
// import is an ordinary create against a restored tree and not a second way to
// bring a sandbox into being.

// ExportSandbox streams the sandbox as a `.dbox` archive: its spec, then its
// durable tree.
//
// The provider refuses a running sandbox, because a tar of a live tree is not a
// consistent one and the reader finds out only on the far side of a transfer.
// Stopping it first is the caller's decision and not something taken on their
// behalf here.
func (s *Service) ExportSandbox(ctx context.Context, projectID, sandboxID string) (io.ReadCloser, error) {
	sb, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	// An archived sandbox exports fine, and deliberately: it is a tree with no
	// container, which is exactly the shape an export reads. Refusing one would
	// mean unarchiving it -- building a container -- to copy data the archive is
	// already holding still.
	if sb.DesiredState == model.DesiredStateDeleted {
		return nil, apperrors.NewStatusError(http.StatusConflict, "sandbox is being deleted")
	}
	// The pool agent refuses a running sandbox too, and it is the authority --
	// it can see the container and this can only see what was last reported.
	// This one exists for the message: the control plane knows the discobox's
	// name, and "stop it first" is worth saying before two network hops rather
	// than after them.
	if sandboxContainerIsLive(sb) {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("%s is %s; stop it before exporting it", sb.Name, sb.RuntimeState))
	}
	manifest, err := s.exportManifest(ctx, sb)
	if err != nil {
		return nil, err
	}
	provider, err := s.resolveProvider(ctx, sb)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, apperrors.NewStatusError(http.StatusConflict, "sandbox has no runtime to export")
	}
	// The pool comes from the row, not from ProviderState. ProviderState is only
	// written once a create has returned a runtime sandbox, so a discobox whose
	// provisioning failed -- an image the pool could not pull, an agent that was
	// not there at create -- has none, and addressing it that way answered 404
	// for exactly the discobox an export is most wanted for: broken here, and
	// worth taking somewhere that works. PoolID is on the row and immutable
	// after create.
	// The pin, not the harness config's current image: the tree is read by the
	// sandbox agent the sandbox runs (ADR 0129 §1).
	image := sandbox.ImageRef{Name: sb.Image, Digest: sb.ImageDigest}
	tree, err := provider.ExportTree(ctx, sandboxRefFromSandbox(sb), sb.PoolID, image, sb.ProviderState)
	if err != nil {
		return nil, err
	}

	reader, writer := io.Pipe()
	go func() {
		defer tree.Close()
		_ = writer.CloseWithError(sandboxexport.Write(writer, manifest, tree))
	}()
	return reader, nil
}

// sandboxContainerIsLive reports whether a container is observed doing
// something, which is the axis an export cares about.
//
// It is not model.SandboxIsLive: that one answers a wider question and counts
// `awaiting_source` as live, which is a lifecycle state rather than a container
// writing to the tree. Empty means no agent has reported yet, which is not
// `stopped` -- but it is also not a container writing, so it does not refuse.
func sandboxContainerIsLive(sb *model.Sandbox) bool {
	switch sb.RuntimeState {
	case model.SandboxRuntimeStateStarting, model.SandboxRuntimeStateRunning, model.SandboxRuntimeStateStopping:
		return sb.State != model.SandboxStateArchived
	default:
		return false
	}
}

// exportManifest is the control-plane half of an export.
func (s *Service) exportManifest(ctx context.Context, sb *model.Sandbox) (*sandboxexport.Manifest, error) {
	harness := sandboxexport.Harness{}
	if sb.HarnessConfigID != nil && strings.TrimSpace(*sb.HarnessConfigID) != "" {
		config, err := s.store.GetHarnessConfig(ctx, sb.ProjectID, *sb.HarnessConfigID)
		if err != nil {
			return nil, apperrors.NotFound(err, "harness config not found")
		}
		harness.Slug, harness.Name = config.Slug, config.Name
	}
	secrets, err := s.exportSecretBindings(ctx, sb)
	if err != nil {
		return nil, err
	}
	spec := sandboxexport.Spec{
		Name:        sb.Name,
		Description: sb.Description,
		Tags:        sb.Tags,
		Harness:     harness,
		Origin:      sb.Origin,
		Manifest:    sb.SandboxManifest,
		Secrets:     secrets,
	}
	// The ID names a row on this server only; the destination resolves the
	// harness by slug (sandboxexport.Spec).
	spec.Manifest.HarnessConfigID = nil

	from := sandboxexport.Source{
		ServerVersion: version.String(),
		ProjectID:     sb.ProjectID,
		SandboxID:     sb.ID,
	}
	if pool, err := s.store.GetPool(ctx, sb.ProjectID, sb.PoolID); err == nil && pool != nil {
		from.PoolName = pool.Name
	}
	return &sandboxexport.Manifest{ExportedAt: time.Now().UTC(), From: from, Sandbox: spec}, nil
}

// exportSecretBindings records which environment variable was bound to which
// named secret, and never a value (ADR 0123 §1).
//
// Two kinds of binding are left out. An agent-requested one is a credential a
// running agent asked for and a human approved, for that sandbox on that server;
// it is not part of what the sandbox is. An anonymous secret has no name to
// resolve on the other side -- it was minted from a value typed at create time
// and is referenced only by ID -- so there is nothing to carry but the value,
// which is the one thing that must not travel.
func (s *Service) exportSecretBindings(ctx context.Context, sb *model.Sandbox) ([]sandboxexport.SecretBinding, error) {
	assignments, err := s.store.ListSandboxSecrets(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		return nil, err
	}
	out := make([]sandboxexport.SecretBinding, 0, len(assignments))
	for i := range assignments {
		assignment := &assignments[i]
		if assignment.AgentRequested {
			continue
		}
		secret, err := s.store.GetSecret(ctx, sb.ProjectID, assignment.SecretID)
		if errors.Is(err, store.ErrNotFound) {
			// A binding whose secret is gone is already not working here; it
			// does not become a reason to refuse the export.
			continue
		}
		if err != nil {
			// Anything else is this server failing to read its own store, and
			// swallowing it would drop the binding from the archive with nothing
			// said: the import would have nothing to match, so it would not warn
			// either, and the discobox would arrive quietly missing a credential.
			return nil, err
		}
		if secret.Anonymous || strings.TrimSpace(secret.Name) == "" {
			continue
		}
		out = append(out, sandboxexport.SecretBinding{Env: assignment.EnvName, Secret: secret.Name})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ImportSandbox creates a discobox from an export stream.
//
// The order is the decision (ADR 0123 §3): the tree is restored onto the pool
// before the row exists, because a row is what wakes the reconciler, and a
// reconciler that runs before the tree lands builds a container around an empty
// workspace. Nothing here parks or waits — by the time anything can observe the
// sandbox, its data is already in place.
func (s *Service) ImportSandbox(ctx context.Context, projectID string, archive io.Reader, opts services.SandboxImportOptions) (*services.SandboxImportResult, error) {
	manifest, tree, err := sandboxexport.Read(archive)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	// Everything that can be refused is refused before the tree is read, so the
	// caller learns their harness is missing after sending a manifest rather
	// than after sending a workspace. The one exception is the name index,
	// which closes a race the check below cannot: two concurrent imports of the
	// same name. Nothing else below reaches the store to refuse.
	defer tree.Close()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	spec := manifest.Sandbox
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = strings.TrimSpace(spec.Name)
	}
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "the archive names no discobox; pass a name")
	}
	taken, err := s.store.SandboxNameTaken(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("a discobox named %q already exists in this project; import it under another name", name))
	}
	poolID := strings.TrimSpace(opts.PoolID)
	if poolID == "" {
		poolID = project.DefaultPoolID
	}
	if poolID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "no pool to import into; name one")
	}
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	harnessConfig, err := s.importHarnessConfig(ctx, projectID, spec.Harness, opts.HarnessSlug)
	if err != nil {
		return nil, err
	}

	if s.sandboxProviders == nil {
		return nil, fmt.Errorf("sandbox provider manager is required")
	}
	instance, err := s.store.GetSandboxProviderInstance(ctx, projectID, pool.ProviderInstanceID)
	if err != nil {
		return nil, apperrors.NotFound(err, "provider instance not found")
	}
	if instance.Disabled {
		return nil, apperrors.NewStatusError(http.StatusConflict, "provider instance disabled")
	}
	provider, err := s.sandboxProviders.ResolveInstance(ctx, instance)
	if err != nil {
		return nil, err
	}
	sandboxID, err := id.New(id.PrefixSandbox)
	if err != nil {
		return nil, err
	}
	sb := &model.Sandbox{
		ID:              sandboxID,
		ProjectID:       projectID,
		CreatedByUserID: s.importingUserID(ctx),
		PoolID:          pool.ID,
		Name:            name,
		Description:     spec.Description,
		SandboxManifest: spec.Manifest,
		// The origin is the client this discobox belongs to, which a move does
		// not change. Carrying it is what gives the imported discobox a source
		// data key -- without which it comes up with no
		// /.discobox/data-per-source/<slug> mount at all -- and what makes
		// `discobox ls` in that repository list it (ADR 0123 §1).
		Origin: spec.Origin,
		// The workspace arrived with the tree, so there is nothing to clone and
		// nothing to wait for a push of. Without this the sandbox would park at
		// `awaiting_source` for a push nobody is going to make (ADR 0123 §4).
		SourceDeliveredAt: importedAt(manifest),
	}
	sb.HarnessConfigID = &harnessConfig.ID
	// The archive's copy of the tags, so the discobox lists and filters by
	// them before it has reported its meta file here (ADR 0136). It is left
	// unobserved, which the discobox's first report always replaces. An
	// archive is not trusted to hold a valid set; one that does not is dropped
	// and waits for that report.
	if len(spec.Tags) > 0 && sandboxmeta.ValidateTags(spec.Tags) == nil {
		sb.Tags = spec.Tags
	}
	// The image is the destination harness config's, exactly as a create takes
	// it from there (ADR 0123 §1). It cannot come from the archive: the rest of
	// what the harness contributes -- RunCommand, Files, Volumes, Env -- is read
	// off this row when the container is built, so an image from one harness
	// and a command from another produce a container that starts and a harness
	// that does not. That is most obvious under `--harness`, and is the same
	// shear in miniature whenever the destination's pin has moved.
	if image := strings.TrimSpace(harnessConfig.Image); image != "" {
		sb.Image, sb.ImageDigest = image, strings.TrimSpace(harnessConfig.ImageDigest)
	}
	if root := sb.Source.Root(); root != "" {
		sb.SourceRoot = &root
	}
	if key := model.SandboxOriginKey(sb.Origin, sb.Source); key != "" {
		sb.OriginKey = &key
	}
	// Delivery is carried through rather than re-decided: it is already written
	// into the bytes that were just restored, and the server's own rule would
	// contradict them (ADR 0123 §4).

	// Every refusal that is left happens here, before the upload. A destination
	// harness with a required secret nobody has bound is an ordinary condition,
	// not a corrupt archive, and answering it after a multi-gigabyte upload --
	// which in a transfer has already stopped the source -- would make the user
	// pay the whole transfer again for it. Only the write is left below.
	assignments, warnings, err := s.importSecretBindings(ctx, projectID, sb, spec.Secrets)
	if err != nil {
		return nil, err
	}
	if sb.HarnessMode != "config" {
		inlineEnvs := make(map[string]struct{}, len(assignments))
		for _, assignment := range assignments {
			inlineEnvs[assignment.EnvName] = struct{}{}
		}
		harnessAssignments, err := s.applyHarnessConfigSecrets(ctx, projectID, sb, harnessConfig.ID, inlineEnvs)
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, harnessAssignments...)
	}

	ref := sandbox.SandboxRef{ProjectID: projectID, SandboxID: sandboxID}
	landedPool, err := provider.ImportTree(ctx, ref, pool.ID, tree)
	if err != nil {
		// The archive failing its own check surfaces here, through the pool
		// agent's request body, and is the uploader's to fix: a 500 naming a
		// pool would send them looking at the destination instead of the file.
		for _, damaged := range []error{tarsums.ErrIncomplete, tarsums.ErrMismatch} {
			if errors.Is(err, damaged) {
				return nil, apperrors.NewStatusError(http.StatusBadRequest,
					fmt.Sprintf("the archive is damaged or incomplete (%v); export it again", damaged))
			}
		}
		return nil, err
	}
	sb.PoolID = landedPool

	created, err := s.createSandboxIntent(ctx, sb, assignments)
	if err != nil {
		return nil, err
	}
	return &services.SandboxImportResult{Sandbox: created, Warnings: warnings}, nil
}

// importedAt is the moment the sources count as delivered. The export's own
// timestamp is used rather than now, so the record says when the workspace was
// captured rather than when it was moved.
func importedAt(manifest *sandboxexport.Manifest) *time.Time {
	at := manifest.ExportedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	return &at
}

// importHarnessConfig resolves the harness the imported sandbox runs.
//
// A slug is the only handle that means the same thing on two servers, and a
// destination that does not have it is told so by name rather than given a
// substitute: the harness is what the sandbox runs, and a different one is a
// different sandbox.
func (s *Service) importHarnessConfig(ctx context.Context, projectID string, harness sandboxexport.Harness, override string) (*model.HarnessConfig, error) {
	slug := strings.TrimSpace(override)
	if slug == "" {
		slug = strings.TrimSpace(harness.Slug)
	}
	if slug == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "the archive names no harness; pass one")
	}
	config, err := s.store.GetHarnessConfigBySlug(ctx, projectID, slug)
	if err != nil {
		wanted := slug
		if harness.Name != "" && slug == harness.Slug {
			wanted = fmt.Sprintf("%s (%s)", slug, harness.Name)
		}
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("this project has no harness %s; configure it here first, or name another with --harness", wanted))
	}
	if !config.Configured {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("harness %q is not configured; run `discobox admin harness configure %s` first", config.Slug, config.Slug))
	}
	return config, nil
}

// importSecretBindings binds each exported environment variable to this
// project's secret of the same name, and reports the ones it could not.
//
// An unbindable one is a warning rather than a refusal. The credential is a
// command away — `discobox admin secret create` — and throwing away a
// transferred workspace to save the user that command is the wrong trade.
func (s *Service) importSecretBindings(ctx context.Context, projectID string, sb *model.Sandbox, bindings []sandboxexport.SecretBinding) ([]*model.SandboxSecret, []string, error) {
	if len(bindings) == 0 {
		return nil, nil, nil
	}
	secrets, err := s.store.ListSecrets(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	byName := make(map[string]*model.Secret, len(secrets))
	for i := range secrets {
		if secrets[i].Anonymous {
			continue
		}
		byName[secrets[i].Name] = &secrets[i]
	}
	var (
		assignments []*model.SandboxSecret
		warnings    []string
		seen        = make(map[string]struct{}, len(bindings))
	)
	for _, binding := range bindings {
		env := strings.TrimSpace(binding.Env)
		if env == "" {
			continue
		}
		if _, dup := seen[env]; dup {
			continue
		}
		seen[env] = struct{}{}
		secret, ok := byName[strings.TrimSpace(binding.Secret)]
		if !ok {
			warnings = append(warnings, fmt.Sprintf(
				"%s was bound to a secret named %q, which this project does not have; the discobox starts without it",
				env, binding.Secret))
			continue
		}
		format := secretFormat(ctx, s.store, secret)
		sentinel, err := secretformat.MintSentinel(format)
		if err != nil {
			return nil, nil, err
		}
		assignments = append(assignments, &model.SandboxSecret{
			ProjectID: projectID,
			SandboxID: sb.ID,
			SecretID:  secret.ID,
			EnvName:   env,
			Sentinel:  sentinel,
			Format:    format,
		})
	}
	return assignments, warnings, nil
}

// importingUserID is who the imported discobox belongs to: the caller, or the
// server's default user when the request carries no principal.
func (s *Service) importingUserID(ctx context.Context) string {
	if userID, err := auth.UserID(ctx); err == nil && strings.TrimSpace(userID) != "" {
		return userID
	}
	return s.defaultUserID
}
