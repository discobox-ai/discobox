package sandboxcreate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/randomname"
)

// PromptOptions describes the client-side inputs used to create a sandbox for
// a prompt. Frontends should populate this type and let this package normalize
// and resolve the request consistently.
type PromptOptions struct {
	Source string
	Ref    string
	// Include names extra sources brought into the sandbox alongside Source,
	// each a directory, or a repository URL, in the same form Source takes. They
	// become the sandbox's source code references.
	Include []string
	// SkipDeclaredSources leaves the sources the primary source's repository
	// declares in .discobox/sources.json out of the sandbox. The zero value
	// brings them in, which is what declaring them asks for; a caller sets this
	// when it wants only what it named itself.
	SkipDeclaredSources bool
	// ReportDeclaredSource is told what each declared source resolved to, for a
	// frontend to show. Leave it nil when there is nowhere to show it.
	ReportDeclaredSource ReportDeclaredSourceFunc
	Prompt               []string
	Env                  []string
	Secret               []string
	Harness              string
	// IncludeDirty decides what happens to uncommitted work in a local source.
	// The zero value is "auto": ask through ConfirmIncludeDirty when there is
	// one, and otherwise include it.
	IncludeDirty IncludeDirty
	// ConfirmIncludeDirty answers the "auto" question for the frontend. Leave it
	// nil when the frontend cannot ask.
	ConfirmIncludeDirty ConfirmIncludeDirtyFunc
	// ConfirmCopyDirectory answers the same "auto" question for a source
	// directory that is in no Git repository, where what is being decided is
	// whether the whole directory is copied in. Leave it nil when the frontend
	// cannot ask.
	ConfirmCopyDirectory ConfirmCopyDirectoryFunc
}

// ParsePromptOptions adds positional prompt arguments and normalizes the source.
func ParsePromptOptions(opts PromptOptions, args []string) (PromptOptions, error) {
	opts.Prompt = append([]string(nil), args...)
	return normalizePromptOptions(opts)
}

func normalizePromptOptions(opts PromptOptions) (PromptOptions, error) {
	if strings.TrimSpace(opts.Source) == "" {
		return PromptOptions{}, errors.New("source directory or Git repository is required")
	}
	if opts.Ref == "" {
		source, ref, ok := splitRunSourceRef(opts.Source)
		if ok {
			opts.Source = source
			opts.Ref = ref
		}
	}
	if opts.IncludeDirty == "" {
		opts.IncludeDirty = IncludeDirtyAuto
	}
	if err := opts.IncludeDirty.Set(string(opts.IncludeDirty)); err != nil {
		return PromptOptions{}, fmt.Errorf("--include-dirty: %w", err)
	}
	return opts, nil
}

// BuildPromptSandboxBody resolves all client-side prompt inputs into the
// control-plane create request, including Git snapshots, local user identity,
// and the local Git authorship the sandbox should commit under.
//
// It also returns the local sources the request was resolved from, which the
// caller closes once they have been delivered — see LocalSources. They are nil
// with an error: nothing survives a build that failed.
func BuildPromptSandboxBody(ctx context.Context, opts PromptOptions) (*apimodel.CreateSandboxBody, *LocalSources, error) {
	opts, err := normalizePromptOptions(opts)
	if err != nil {
		return nil, nil, err
	}
	name, err := randomname.Generate()
	if err != nil {
		return nil, nil, fmt.Errorf("generate discobox name: %w", err)
	}
	body := &apimodel.CreateSandboxBody{Config: apimodel.SandboxCreateConfig{Name: name}}
	if len(opts.Prompt) > 0 {
		body.Config.SetPrompt(append([]string(nil), opts.Prompt...))
	}
	if strings.TrimSpace(opts.Harness) != "" {
		body.SetHarnessName(optionalString(opts.Harness))
	}
	env, secrets, err := EnvAndSecretsFromOptions(opts.Env, opts.Secret)
	if err != nil {
		return nil, nil, err
	}
	if len(env) > 0 {
		body.Config.SetEnv(apiclientgen.NewOptSandboxCreateConfigEnv(apiclientgen.SandboxCreateConfigEnv(env)))
	}
	if len(secrets) > 0 {
		body.Config.SetSecrets(secrets)
	}
	userIdentity, _, err := resolveRunUserIdentity()
	if err != nil {
		return nil, nil, err
	}
	sourceArg := opts.Source
	if opts.Ref != "" {
		sourceArg += "@" + opts.Ref
	}
	sourceOptions := runSourceOptions{
		IncludeDirty: opts.IncludeDirty,
		Confirm:      opts.ConfirmIncludeDirty,
		ConfirmCopy:  opts.ConfirmCopyDirectory,
	}
	source, err := resolveRunSource(ctx, sourceArg, sourceOptions)
	if err != nil {
		return nil, nil, err
	}
	local := &LocalSources{}
	local.add("", source)
	apiSource, err := source.apiGitSource()
	if err != nil {
		local.Close()
		return nil, nil, err
	}
	body.Config.SetSource(apiclientgen.NewOptGitSource(*apiSource))
	if err := setSourceCodeReferences(ctx, body, local, opts, source, sourceOptions); err != nil {
		local.Close()
		return nil, nil, err
	}
	resolvedOrigin, err := ResolveOrigin(ctx, sourceArg)
	if err != nil {
		local.Close()
		return nil, nil, err
	}
	body.SetOrigin(apiclientgen.NewOptOrigin(resolvedOrigin))
	userIdentity.setCreateSandboxUser(body)
	setCreateSandboxGit(body, resolveGitIdentity(ctx, sourceArg))
	return body, local, nil
}

// setSourceCodeReferences resolves the extra sources and files them on the
// create request, recording each one's local repository so delivery can push
// out of it.
//
// Two things put a source here: `--include`, which the caller named, and the
// primary source's own .discobox/sources.json, which the repository declared.
// The caller's win — an explicitly named source is not overridden by a
// declaration of the same thing — and are resolved first for that reason.
//
// Every reference is resolved the same way the primary source was, uncommitted
// work and all: each is a separate working tree, so each gets its own answer to
// the dirty-workspace question rather than inheriting the primary source's.
//
// The reference key is the sandbox directory the source lands in, so two
// sources that would land on top of each other are refused rather than silently
// collapsed into one — including a reference that resolves to the primary
// source's own directory.
func setSourceCodeReferences(ctx context.Context, body *apimodel.CreateSandboxBody, local *LocalSources, opts PromptOptions, primary resolvedRunSource, sourceOptions runSourceOptions) error {
	declared, err := declaredPromptSources(opts, primary)
	if err != nil {
		return err
	}
	if len(opts.Include) == 0 && len(declared) == 0 {
		return nil
	}
	references := make(apiclientgen.SandboxCreateConfigSourceCodeReferences, len(opts.Include)+len(declared))
	// The server names the primary source "primary", so no reference may take
	// that name either.
	used := map[string]struct{}{"primary": {}}
	add := func(reference resolvedReference) {
		local.add(reference.Key, reference.Resolved)
		references[reference.Key] = *reference.APISource
	}
	for _, arg := range opts.Include {
		if strings.TrimSpace(arg) == "" {
			return errors.New("--include needs a source directory or Git repository")
		}
		reference, err := resolveNamedReference(ctx, arg, referencePlacement{}, sourceOptions, used)
		if err != nil {
			return err
		}
		if taken(references, primary, reference.Key) {
			reference.Resolved.close()
			return fmt.Errorf("--include %s resolves to %s, which is already included", arg, reference.Key)
		}
		add(reference)
	}
	// A declared source is looked for on this machine and placed in the
	// sandbox, which are two different paths: the checkout beside the primary
	// source is found on the host filesystem, and where a clone lands is named
	// in the sandbox's. They read the same on a POSIX host, where the primary
	// source keeps its own path, and differ on Windows, where it is mirrored
	// under /mnt.
	primaryRoot := primary.LocalDirectory
	sandboxRoot := path.Dir(filepath.ToSlash(primary.Destination.Directory))
	for _, name := range declaredSourceNames(declared) {
		arg, report := resolveDeclaredSourceArg(ctx, primaryRoot, name, declared[name])
		placement := referencePlacement{Name: name, Root: sandboxRoot}
		reference, err := resolveNamedReference(ctx, arg, placement, sourceOptions, used)
		if err != nil {
			return fmt.Errorf("declared source %q (%s): %w", name, declared[name], err)
		}
		if taken(references, primary, reference.Key) {
			// Already brought in by --include, or the sandbox's own source. The
			// declaration asked for it to be there, and it is.
			reference.Resolved.close()
			delete(used, reference.Resolved.Slug)
			continue
		}
		add(reference)
		if opts.ReportDeclaredSource != nil {
			opts.ReportDeclaredSource(report)
		}
	}
	if len(references) > 0 {
		body.Config.SetSourceCodeReferences(apiclientgen.NewOptSandboxCreateConfigSourceCodeReferences(references))
	}
	return nil
}

// resolveNamedReference resolves one extra source and builds its API shape, so
// a caller that decides not to keep it can close it without having half-filed
// it on the request.
func resolveNamedReference(ctx context.Context, arg string, placement referencePlacement, opts runSourceOptions, used map[string]struct{}) (resolvedReference, error) {
	reference, err := resolveRunSourceReference(ctx, arg, placement, opts, used)
	if err != nil {
		return resolvedReference{}, err
	}
	apiSource, err := reference.Resolved.apiGitSource()
	if err != nil {
		reference.Resolved.close()
		return resolvedReference{}, err
	}
	reference.APISource = apiSource
	return reference, nil
}

// taken reports whether something already occupies the sandbox directory a
// reference wants.
func taken(references apiclientgen.SandboxCreateConfigSourceCodeReferences, primary resolvedRunSource, key string) bool {
	if _, ok := references[key]; ok {
		return true
	}
	return key == primary.Destination.Directory
}

// declaredPromptSources are the sources the primary source's repository
// declares, or nothing when the caller opted out or the source is not one this
// machine holds.
//
// The file is read out of the source's directory on this machine, not the one
// it takes in the sandbox: the sandbox does not exist yet, and off a POSIX host
// the two are not even the same path.
//
// A remote primary source declares nothing here: the file lives in a checkout,
// and there is none — reading it would mean cloning the repository on this
// machine first, which is the sandbox's job, not the client's.
func declaredPromptSources(opts PromptOptions, primary resolvedRunSource) (map[string]string, error) {
	if opts.SkipDeclaredSources || primary.LocalDirectory == "" || primary.URL != "" {
		return nil, nil
	}
	return readDeclaredSources(primary.LocalDirectory)
}

type promptSandboxCreator interface {
	CreateSandbox(context.Context, *apimodel.CreateSandboxBody, apiclientgen.CreateSandboxParams) (apiclientgen.CreateSandboxRes, error)
}

// nameConflictAttempts bounds the retries below. Sandbox names are unique
// within a project and this one was generated rather than chosen, so a
// collision is ours to resolve silently — but only a handful of times, since a
// persistent 409 means something other than bad luck.
const nameConflictAttempts = 5

// CreatePromptSandbox builds, submits, and decodes a prompt sandbox request,
// picking another name if the generated one is already taken, and returns the
// local sources it was resolved from for the caller to deliver and then close.
// They are nil with an error.
//
// Only this path retries: the name here is generated (BuildPromptSandboxBody),
// so replacing it costs the caller nothing. A name the user typed is theirs,
// and `admin discobox create --name` reports the conflict instead.
func CreatePromptSandbox(ctx context.Context, client promptSandboxCreator, projectID string, opts PromptOptions, report Report) (*apimodel.Sandbox, *LocalSources, error) {
	report.step(StepPreparingSource)
	body, local, err := BuildPromptSandboxBody(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	report.step(StepCreating)
	for attempt := 1; ; attempt++ {
		res, err := client.CreateSandbox(ctx, body, apiclientgen.CreateSandboxParams{ProjectId: projectID})
		if err != nil {
			local.Close()
			return nil, nil, err
		}
		if sandbox, ok := res.(*apimodel.Sandbox); ok {
			return sandbox, local, nil
		}
		if attempt >= nameConflictAttempts || !isNameConflict(res) {
			local.Close()
			return nil, nil, createResponseError(res)
		}
		name, err := randomname.Generate()
		if err != nil {
			local.Close()
			return nil, nil, fmt.Errorf("generate discobox name: %w", err)
		}
		body.Config.Name = name
	}
}

// isNameConflict reports whether res is the conflict a duplicate sandbox name
// produces. Create has no other 409, so the status alone identifies it.
func isNameConflict(res any) bool {
	problem, ok := res.(*apiclientgen.ErrorModelStatusCode)
	return ok && problem.StatusCode == http.StatusConflict
}

func createResponseError(res any) error {
	if problem, ok := res.(*apiclientgen.ErrorModelStatusCode); ok {
		status := problem.StatusCode
		title := problem.Response.Title.Value
		detail := problem.Response.Detail.Value
		switch {
		case title != "" && detail != "":
			return fmt.Errorf("request failed: %d %s: %s", status, title, detail)
		case title != "":
			return fmt.Errorf("request failed: %d %s", status, title)
		case detail != "":
			return fmt.Errorf("request failed: %d %s", status, detail)
		case status != 0:
			return fmt.Errorf("request failed: %d %s", status, http.StatusText(status))
		}
	}
	if problem, ok := res.(*apiclientgen.ErrorResponseStatusCode); ok {
		status := problem.StatusCode
		if problem.Response.Error != "" {
			return fmt.Errorf("request failed: %d %s", status, problem.Response.Error)
		}
		if status != 0 {
			return fmt.Errorf("request failed: %d %s", status, http.StatusText(status))
		}
	}
	return fmt.Errorf("unexpected response type %T", res)
}

func optionalString(value string) apiclientgen.OptString {
	if strings.TrimSpace(value) == "" {
		return apiclientgen.OptString{}
	}
	return apiclientgen.NewOptString(value)
}
