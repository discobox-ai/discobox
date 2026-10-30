package sandboxcreate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/origin"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/randomname"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/sandboxpath"
	"github.com/discobox-ai/discobox/pkg/sandboxuser"
	"github.com/discobox-ai/discobox/pkg/sourceref"
)

// PromptOptions describes the client-side inputs used to create a sandbox for
// a prompt. Frontends should populate this type and let this package normalize
// and resolve the request consistently.
type PromptOptions struct {
	// PoolID is the pool the discobox is placed on. Empty leaves it to the
	// server, which places it on the project's default pool.
	PoolID string
	// Platform is what that pool hosts, which the discobox runs on. Every path
	// this package places in it — where a source lands, the user's home — is
	// judged by it (ADR 0145 §6), so a frontend reads it from the pool before
	// building a request.
	Platform platform.Platform
	// Source is the primary source, `LOCATION[@REF][//PATH]` (ADR
	// 26-10-15-664): //PATH is the directory inside it the harness starts in.
	Source string
	// NoSource creates the sandbox with nothing materialized in it. Source is
	// then only where the request came from — the origin the sandbox is filed
	// under and the Git identity it commits as — and nothing is cut from it.
	NoSource bool
	// Include names extra sources brought into the sandbox alongside Source,
	// each a directory, or a repository URL, in the same form Source takes. They
	// become the sandbox's source code references.
	Include []string
	// SkipDeclaredSources leaves the sources the primary source's repository
	// declares in .discobox/settings.json out of the sandbox. The zero value
	// brings them in, which is what declaring them asks for; a caller sets this
	// when it wants only what it named itself.
	SkipDeclaredSources bool
	// ReportDeclaredSource is told what each declared source resolved to, for a
	// frontend to show. Leave it nil when there is nowhere to show it.
	ReportDeclaredSource ReportDeclaredSourceFunc
	// Warn is told what the caller should change about what the request was
	// built from — a repository whose settings are read from the files
	// .discobox/settings.json replaces. Leave it nil when there is nowhere to
	// show it.
	Warn func(string)
	// ReportInlineSecret is told, once the create has succeeded, for each
	// value it stored as a secret of its own, where it is sent ("DB_PASS: stored, sent to
	// corp.example"), so an @ read as a host is seen rather than discovered.
	// Leave it nil when there is nowhere to show it.
	ReportInlineSecret func(string)
	Prompt             []string
	Env                []string
	Secret             []string
	// SkillDirs and UserSkills are --skills and --user-skills: directories of
	// skills read into the request, and whether ~/.claude/skills and
	// ~/.agents/skills are read first (ReadSkills).
	SkillDirs  []string
	UserSkills bool
	// ProjectSkills are the project's skills sources, added ahead of the
	// skills sources of SkillDirs so a person's own win on a skill's name (ADR
	// 26-10-14-894 §2). The frontend reads them from the server.
	ProjectSkills []SkillsRepository
	// Bindings are the secrets bound to the new discobox at create (ADR
	// 26-10-27-880 §5), as ParseBindings reads them from --bind and
	// --delegate. The server reads each secret by name or ID.
	Bindings []apimodel.SandboxBindingInput
	// DiscoboxAPI lets the new discobox call the discobox API through its
	// pool (ADR 26-10-27-880 §9).
	DiscoboxAPI bool
	Harness     string
	// IncludeDirty decides what happens to uncommitted work on top of a
	// local source's checked-out commit. Asking is the frontend's, before it
	// gets here: `discobox run` settles it from --include-dirty, and the
	// console from its dialog. The zero value is "auto", which includes the
	// work — what a source the console's one question did not cover, an -i
	// dirty while the primary is clean, has always meant.
	IncludeDirty IncludeDirty
	// ConfirmCopyDirectory answers the same "auto" question for a source
	// directory that is in no Git repository, where what is being decided is
	// whether the whole directory is copied in. Leave it nil when the frontend
	// cannot ask.
	ConfirmCopyDirectory ConfirmCopyDirectoryFunc
	// CopyDirectory answers that question ahead of time: true copies the
	// directory in, false leaves the discobox with no source. Nil asks through
	// ConfirmCopyDirectory, and with nobody to ask the directory is copied. It
	// is an answer of its own rather than IncludeDirty's, because a directory
	// in no repository has no commit to start from instead: the only real
	// choices are its content or nothing.
	CopyDirectory *bool
}

// ParsePromptOptions adds positional prompt arguments and normalizes the source.
func ParsePromptOptions(opts PromptOptions, args []string) (PromptOptions, error) {
	opts.Prompt = append([]string(nil), args...)
	return normalizePromptOptions(opts)
}

func normalizePromptOptions(opts PromptOptions) (PromptOptions, error) {
	if opts.NoSource {
		// A ref names a commit to check out, and there is nothing to check it
		// out of. Saying both is a contradiction rather than a preference, so
		// it is refused instead of one half being dropped silently.
		if source, err := sourceref.Parse(opts.Source); err == nil && source.Ref != "" {
			return PromptOptions{}, errors.New("--no-source takes no ref: there is no source to check one out of")
		}
	} else if strings.TrimSpace(opts.Source) == "" {
		return PromptOptions{}, errors.New("source directory or Git repository is required")
	}
	if opts.IncludeDirty == "" {
		opts.IncludeDirty = IncludeDirtyAuto
	}
	switch opts.IncludeDirty {
	case IncludeDirtyAuto, IncludeDirtyAlways, IncludeDirtyNever:
	default:
		return PromptOptions{}, fmt.Errorf("include dirty: invalid value %q: want true, false, or auto", opts.IncludeDirty)
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
	body.Bindings = opts.Bindings
	if opts.DiscoboxAPI {
		body.SetDiscoboxApi(apiclientgen.NewOptBool(true))
	}
	skillsArgs, err := SplitSkills(opts.SkillDirs)
	if err != nil {
		return nil, nil, err
	}
	skills, err := ReadSkills(skillsArgs.Directories, opts.UserSkills)
	if err != nil {
		return nil, nil, err
	}
	SetCreateSandboxSkills(&body.Config, skills)
	if poolID := strings.TrimSpace(opts.PoolID); poolID != "" {
		body.SetPoolId(apiclientgen.NewOptString(poolID))
	}
	// This machine's account is asked for only where the sandbox has POSIX
	// ids. Elsewhere it has one account, the one its template provisions, and
	// a user naming a uid, a group or a home is refused there — even the
	// account's own (ADR 0145 §5) — so none is sent and the sandbox runs as
	// that account.
	var userIdentity runUserIdentity
	userNamed := false
	if sandboxuser.HasPOSIXIDs(opts.Platform.OS) {
		if userIdentity, userNamed, err = resolveRunUserIdentity(); err != nil {
			return nil, nil, err
		}
	}
	sourceArg := opts.Source
	sourceOptions := runSourceOptions{
		Platform:      opts.Platform,
		IncludeDirty:  opts.IncludeDirty,
		ConfirmCopy:   opts.ConfirmCopyDirectory,
		CopyDirectory: opts.CopyDirectory,
		Warn:          opts.Warn,
	}
	local := &LocalSources{}
	// A sandbox with no source has no primary source to resolve, and the zero
	// value stands for it throughout: nothing local to deliver, nothing to
	// place references beside, and no repository to read declared sources out
	// of. What Source still says is where the request came from, which is the
	// origin and the Git authorship below.
	var source resolvedRunSource
	if !opts.NoSource {
		if source, err = resolveRunSource(ctx, sourceArg, sourceOptions); err != nil {
			return nil, nil, err
		}
	}
	// Resolution answers "no source" too: declining to copy a directory that is
	// in no repository leaves nothing to check out, and asking for that by
	// answering the question is the same request as asking for it by flag
	// (ADR 0077 §1).
	if source.resolved() {
		local.add("", source)
	}
	skillsRepos := append(slices.Clone(opts.ProjectSkills), skillsArgs.Repositories...)
	if err := setSourceCodeReferences(ctx, body, local, opts, &source, sourceOptions, skillsRepos); err != nil {
		local.Close()
		return nil, nil, err
	}
	// Filed after the references, which may move a primary source placed by
	// name off a directory one of them claims (yieldPrimaryDirectory).
	if source.resolved() {
		apiSource, err := source.apiGitSource()
		if err != nil {
			local.Close()
			return nil, nil, err
		}
		body.Config.SetSource(apiclientgen.NewOptGitSource(*apiSource))
	}
	resolvedOrigin, err := origin.Resolve()
	if err != nil {
		local.Close()
		return nil, nil, err
	}
	body.SetOrigin(apiclientgen.NewOptOrigin(resolvedOrigin))
	// Root asks for nobody: the image's own user (ADR 0025 §5).
	if userNamed {
		userIdentity.setCreateSandboxUser(body)
	}
	setCreateSandboxGit(body, resolveGitIdentity(ctx, sourceArg))
	return body, local, nil
}

// setSourceCodeReferences resolves the extra sources and files them on the
// create request, recording each one's local repository so delivery can push
// out of it.
//
// Two things put a source here: `--include`, which the caller named, and the
// primary source's own .discobox/settings.json, which the repository declared.
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
// source's own directory, unless the primary is a remote one placed there only
// by its name, when the primary moves to the default directory instead
// (heldAgainst, yieldPrimaryDirectory) — except for a declared source naming
// the primary's own repository, which is the primary itself and is skipped.
//
// The skills repositories of --skills come last, each a reference marked for
// its skills (setSkillsSources).
//
// The same settings say which of the box's features the repository turns off,
// which the request carries too: the client is their reader, as it is the
// sources' (ADR 26-10-16-423 §11).
func setSourceCodeReferences(ctx context.Context, body *apimodel.CreateSandboxBody, local *LocalSources, opts PromptOptions, primary *resolvedRunSource, sourceOptions runSourceOptions, skillsRepos []SkillsRepository) error {
	settings, err := declaredPromptSettings(ctx, opts, *primary)
	if err != nil {
		return err
	}
	if len(settings.Features) > 0 {
		features := make(apiclientgen.SandboxCreateConfigFeatures, len(settings.Features))
		for name, feature := range settings.Features {
			features[name] = apiclientgen.SandboxFeature{Enabled: feature.Enabled}
		}
		body.Config.SetFeatures(apiclientgen.NewOptSandboxCreateConfigFeatures(features))
	}
	declared := settings.Sources
	if len(opts.Include) == 0 && len(declared) == 0 && len(skillsRepos) == 0 {
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
		// The copy of a directory in no repository was declined, so this source
		// is left out rather than brought in empty (ADR 0077 §4).
		if !reference.Resolved.resolved() {
			continue
		}
		if taken(references, primary.heldAgainst(opts.Platform, false), reference.Key) {
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
	// under /mnt, and whenever the sandbox is not Linux, where it is placed by
	// name.
	primaryRoot := primary.LocalDirectory
	sandboxRoot := ""
	if primary.Destination.Directory != "" {
		sandboxRoot = sandboxpath.For(opts.Platform).Dir(primary.Destination.Directory)
	}
	for _, name := range declaredSourceNames(declared) {
		arg, report := resolveDeclaredSourceArg(ctx, primaryRoot, name, declared[name])
		placement := referencePlacement{Name: name, Root: sandboxRoot}
		reference, err := resolveNamedReference(ctx, arg, placement, sourceOptions, used)
		if err != nil {
			return fmt.Errorf("declared source %q (%s): %w", name, declared[name], err)
		}
		if !reference.Resolved.resolved() {
			continue
		}
		if taken(references, primary.heldAgainst(opts.Platform, sameGitRemote(primary.URL, reference.Resolved.URL)), reference.Key) {
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
	// A skills source is placed under its own directory, never by name beside
	// the primary, so what the primary holds against it is what it holds
	// against any other repository.
	held := primary.heldAgainst(opts.Platform, false)
	if err := setSkillsSources(ctx, &body.Config, references, local, skillsRepos, sourceOptions, used, func(key string) bool {
		return taken(references, held, key)
	}); err != nil {
		return err
	}
	return yieldPrimaryDirectory(primary, references, opts.Platform)
}

// resolveNamedReference resolves one extra source and builds its API shape, so
// a caller that decides not to keep it can close it without having half-filed
// it on the request.
func resolveNamedReference(ctx context.Context, arg string, placement referencePlacement, opts runSourceOptions, used map[string]struct{}) (resolvedReference, error) {
	reference, err := resolveRunSourceReference(ctx, arg, placement, opts, used)
	if err != nil {
		return resolvedReference{}, err
	}
	if !reference.Resolved.resolved() {
		// Left out rather than resolved: there is no source to build a shape of.
		return resolvedReference{}, nil
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
// reference wants: another reference, or held, the primary source's.
func taken(references apiclientgen.SandboxCreateConfigSourceCodeReferences, held, key string) bool {
	if _, ok := references[key]; ok {
		return true
	}
	return held != "" && key == held
}

// declaredSettings is what the client reads of the primary source's
// repository settings.
type declaredSettings struct {
	// Sources are the sources it declares, checked, or nothing when the
	// caller opted out of them.
	Sources map[string]string
	// Features are its features, which a box provisions unless they are
	// turned off.
	Features map[string]sandboxconfig.Feature
}

// declaredPromptSettings is what the primary source's repository settings
// declare: its sources, unless the caller opted out of them, and its features.
//
// For a source on this machine the file is read out of its directory here,
// not the one it takes in the sandbox: the sandbox does not exist yet, and off
// a POSIX host the two are not even the same path. For a remote primary source
// it is read from the remote, at the commit the source is pinned to, which
// takes one small fetch and no clone (ADR 26-10-10-545 §4).
//
// A repository read through the files settings.json replaces, or whose
// settings.json sets keys this client does not know, is warned about (Warn): they are still read, since they live in repositories nobody here can
// migrate, but the person creating the box is the one who can.
func declaredPromptSettings(ctx context.Context, opts PromptOptions, primary resolvedRunSource) (declaredSettings, error) {
	var settings sandboxconfig.RepositorySettings
	var where string
	var err error
	switch {
	case primary.LocalDirectory != "":
		where = primary.LocalDirectory
		settings, err = readLocalSettings(where)
	case primary.URL != "" && primary.Checkout.Commit != "":
		where = primary.URL + "@" + primary.Checkout.Commit[:min(len(primary.Checkout.Commit), 12)]
		settings, err = readRemoteSettings(ctx, primary.URL, primary.Checkout.Commit, primary.Checkout.RefName)
	default:
		return declaredSettings{}, nil
	}
	if err != nil {
		return declaredSettings{}, err
	}
	parsed, err := settings.Settings()
	if err != nil {
		return declaredSettings{}, fmt.Errorf("%s: %w", where, err)
	}
	declared := declaredSettings{Features: parsed.Features}
	if opts.SkipDeclaredSources {
		return declared, nil
	}
	if opts.Warn != nil {
		if warning := settings.LegacyWarning(); warning != "" {
			opts.Warn(where + ": " + warning)
		}
		// Read past rather than refused, so a key a newer Discobox added does
		// not stop this one; said, so a misspelled one is seen.
		if unknown := settings.UnknownKeys(); len(unknown) > 0 {
			opts.Warn(fmt.Sprintf("%s: %s sets %s, which this discobox does not read; misspelled, or for a newer discobox",
				where, sandboxconfig.SettingsFile, quoteKeys(unknown)))
		}
	}
	declared.Sources, err = declaredSources(settings, where)
	return declared, err
}

type promptSandboxCreator interface {
	CreateSandbox(context.Context, *apimodel.CreateSandboxBody, apiclientgen.CreateSandboxParams) (apiclientgen.CreateSandboxRes, error)
	secretReader
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
// and `admin box create --name` reports the conflict instead.
func CreatePromptSandbox(ctx context.Context, client promptSandboxCreator, projectID string, opts PromptOptions, report Report) (*apimodel.Sandbox, *LocalSources, error) {
	report.step(StepPreparingSource)
	body, local, err := BuildPromptSandboxBody(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	if err := NameBindingVariables(ctx, client, projectID, body.Bindings); err != nil {
		local.Close()
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
			// Said only now: a create that failed stored nothing.
			if opts.ReportInlineSecret != nil {
				for _, secret := range body.Config.Secrets {
					opts.ReportInlineSecret(InlineSecretLine(secret))
				}
			}
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
