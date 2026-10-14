package sandboxcreate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxpath"
)

// skillsSourcesDirectory is where a skills source is checked out, under the
// sandbox's working root: apart from the sources being worked on (ADR
// 26-10-14-294 §2).
const skillsSourcesDirectory = "skill-sources"

// SkillsArgs are --skills values split by their form (ADR 26-10-14-294 §1):
// the directories read as content, and the Git URLs that are skills sources,
// each in the order given.
type SkillsArgs struct {
	Directories  []string
	Repositories []SkillsRepository
}

// SkillsRepository is one --skills Git URL: the repository, at Ref when one
// was named, and the directory inside it that holds the skills.
type SkillsRepository struct {
	// Arg is the value as given, for messages.
	Arg string
	// Source is the repository with its @REF, as a source argument takes it.
	Source string
	// Directory is the skills directory inside it, RootSkillsDirectory for the
	// root.
	Directory string
}

// SplitSkills splits --skills values into directories and skills sources. A
// value is a skills source when what precedes its //PATH is a Git URL; any
// other value is a directory, whether or not it is a Git checkout.
func SplitSkills(values []string) (SkillsArgs, error) {
	var out SkillsArgs
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return SkillsArgs{}, errors.New("--skills needs a directory or a Git URL")
		}
		repo, ok, err := parseSkillsRepository(value)
		if err != nil {
			return SkillsArgs{}, fmt.Errorf("--skills %s: %w", value, err)
		}
		if ok {
			out.Repositories = append(out.Repositories, repo)
		} else {
			out.Directories = append(out.Directories, value)
		}
	}
	return out, nil
}

// IsSkillsRepository reports whether a --skills value is a skills source
// rather than a directory: a Git URL, with or without its //PATH. One whose
// PATH SplitSkills refuses is still one, so the refusal names the PATH
// rather than a directory nobody meant.
func IsSkillsRepository(value string) bool {
	_, ok, err := parseSkillsRepository(value)
	return ok || err != nil
}

// parseSkillsRepository reads URL[@REF][//PATH]. The path starts at the first
// `//` after the URL's `://`, if it has one: Git refuses consecutive slashes
// in a ref name, and a repository URL has none after its scheme, so what
// precedes it is URL[@REF] as a source takes it, and the path may hold `@` and
// `:`. ok is false for a value that is not a Git URL.
func parseSkillsRepository(value string) (SkillsRepository, bool, error) {
	value = strings.TrimSpace(value)
	start := 0
	if i := strings.Index(value, "://"); i >= 0 {
		start = i + len("://")
	}
	source, dir := value, sandboxconfig.RootSkillsDirectory
	if i := strings.Index(value[start:], "//"); i >= 0 {
		source, dir = value[:start+i], value[start+i+len("//"):]
	}
	if base, _, _ := splitRunSourceRef(source); !IsRemoteGitSource(base) {
		return SkillsRepository{}, false, nil
	}
	// Past here the value is a Git URL, so an error is about its PATH.
	if err := sandboxconfig.ValidateSkillsDirectory(dir); err != nil {
		return SkillsRepository{}, false, err
	}
	return SkillsRepository{Arg: value, Source: source, Directory: dir}, true, nil
}

// setSkillsSources resolves each skills repository as an extra source marked
// for its skills, and files the directories in the order they were named, so
// the sandbox links a later one over an earlier one (ADR 26-10-14-294 §§1–2).
//
// Directories of one repository at one ref share its checkout. One
// repository at two refs is refused: it would need two checkouts of one name.
func setSkillsSources(ctx context.Context, config *apimodel.SandboxCreateConfig, references apiclientgen.SandboxCreateConfigSourceCodeReferences, local *LocalSources, repos []SkillsRepository, sourceOptions runSourceOptions, used map[string]struct{}, occupied func(key string) bool) error {
	if len(repos) == 0 {
		return nil
	}
	paths := sandboxpath.For(sourceOptions.Platform)
	placement := referencePlacement{Root: paths.Join(paths.WorkingRoot(), skillsSourcesDirectory)}
	keys := map[string]string{}
	refs := map[string]string{}
	var skillsSources []apiclientgen.SandboxSkillsSource
	for _, repo := range repos {
		base, _, _ := splitRunSourceRef(repo.Source)
		key, resolved := keys[repo.Source]
		if !resolved {
			if other, ok := refs[base]; ok {
				return fmt.Errorf("--skills %s and %s name one repository at two refs; a discobox checks out one", other, repo.Arg)
			}
			reference, err := resolveNamedReference(ctx, repo.Source, placement, sourceOptions, used)
			if err != nil {
				return fmt.Errorf("--skills %s: %w", repo.Arg, err)
			}
			if occupied(reference.Key) {
				reference.Resolved.close()
				return fmt.Errorf("--skills %s lands at %s, where another source already is", repo.Arg, reference.Key)
			}
			reference.APISource.SetPurpose(apiclientgen.NewOptGitSourcePurpose(apiclientgen.GitSourcePurposeSkills))
			local.add(reference.Key, reference.Resolved)
			references[reference.Key] = *reference.APISource
			key = reference.Key
			keys[repo.Source] = key
			refs[base] = repo.Arg
		}
		skillsSources = append(skillsSources, apiclientgen.SandboxSkillsSource{Source: key, Directory: repo.Directory})
	}
	config.SetSourceCodeReferences(apiclientgen.NewOptSandboxCreateConfigSourceCodeReferences(references))
	config.SkillsSources = skillsSources
	return nil
}

// AddSkillsSources resolves skills repositories onto a create request whose
// other sources were named outright, as `sandbox create` names them: beside
// the references already on it, refusing one that lands where one of them
// is. A skills source is a remote URL, so nothing local is left to deliver
// from.
func AddSkillsSources(ctx context.Context, config *apimodel.SandboxCreateConfig, repos []SkillsRepository) error {
	if len(repos) == 0 {
		return nil
	}
	references, _ := config.SourceCodeReferences.Get()
	if references == nil {
		references = apiclientgen.SandboxCreateConfigSourceCodeReferences{}
	}
	used := map[string]struct{}{sandboxconfig.PrimarySourceSlug: {}}
	for _, reference := range references {
		if slug, ok := reference.Slug.Get(); ok {
			used[slug] = struct{}{}
		}
	}
	primaryDirectory := ""
	if source, ok := config.Source.Get(); ok {
		if destination, ok := source.Destination.Get(); ok {
			primaryDirectory = destination.Directory.Or("")
		}
	}
	local := &LocalSources{}
	defer local.Close()
	return setSkillsSources(ctx, config, references, local, repos, runSourceOptions{}, used, func(key string) bool {
		_, ok := references[key]
		return ok || key == primaryDirectory
	})
}
