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
	"github.com/discobox-ai/discobox/sourceref"
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
	// Source is the repository with its @REF, and no //PATH: Directory is
	// that.
	Source sourceref.Ref
	// Directory is the skills directory inside it, RootSkillsDirectory for the
	// root.
	Directory string
	// Project marks an entry of the project's skills sources, which the
	// create copies in ahead of its own --skills (ADR 26-10-14-894 §2).
	Project bool
}

// ProjectSkillsRepository is one entry of the project's skills sources as the
// --skills URL[@REF][//PATH] it stands for, and the refusal the entry earns
// (sandboxconfig.ValidateSkillsRepository, ValidateSkillsDirectory). The entry
// is what the server answered, and another member wrote it: it is checked
// here again, before Git runs on it. The repository comes back either way,
// for a listing to show what the project names.
func ProjectSkillsRepository(url, ref, directory string) (SkillsRepository, error) {
	if directory == "" {
		directory = sandboxconfig.RootSkillsDirectory
	}
	source := sourceref.Ref{Kind: sourceref.Git, Location: url, Ref: ref}
	repo := SkillsRepository{Arg: skillsArg(source, directory), Source: source, Directory: directory, Project: true}
	if err := sandboxconfig.ValidateSkillsRepository(url, ref); err != nil {
		return repo, fmt.Errorf("%s: %w", repo, err)
	}
	if err := sandboxconfig.ValidateSkillsDirectory(directory); err != nil {
		return repo, fmt.Errorf("%s: %w", repo, err)
	}
	return repo, nil
}

// skillsArg is a repository and its skills directory as one --skills value.
func skillsArg(source sourceref.Ref, directory string) string {
	return skillsRef(source, directory).String()
}

// skillsRef is a repository with its skills directory as its //PATH.
func skillsRef(source sourceref.Ref, directory string) sourceref.Ref {
	if directory != sandboxconfig.RootSkillsDirectory {
		source.Path = directory
	}
	return source
}

// Value is the skills source as a --skills value, with GitHub shorthand
// expanded: what a create reads the same way from any directory.
func (r SkillsRepository) Value() string {
	return skillsArg(r.Source, r.Directory)
}

// Key tells one skills source from another, however its URL was spelled: the
// repository, its ref and its directory (sourceref.Ref.Key).
func (r SkillsRepository) Key() string {
	return skillsRef(r.Source, r.Directory).Key()
}

// Display is the skills source for a person to read and type back
// (sourceref.Ref.Display), with a directory under home from `~`.
func (r SkillsRepository) Display(home string) string {
	return skillsRef(r.Source, r.Directory).Display(home)
}

// String names the repository the way the person creating the discobox named
// it: by its --skills value, or as the project's.
func (r SkillsRepository) String() string {
	if r.Project {
		return "the project's skills source " + r.Arg
	}
	return "--skills " + r.Arg
}

// SplitSkills splits --skills values into directories and skills sources. A
// value is a skills source when it names a Git repository — a URL, or GitHub
// shorthand that is not a directory here; any other value is a directory,
// whether or not it is a Git checkout.
func SplitSkills(values []string) (SkillsArgs, error) {
	var out SkillsArgs
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return SkillsArgs{}, errors.New("--skills needs a directory or a Git URL")
		}
		repo, dir, err := ParseSkills(value, "")
		if err != nil {
			return SkillsArgs{}, fmt.Errorf("--skills %s: %w", value, err)
		}
		if dir == "" {
			out.Repositories = append(out.Repositories, repo)
		} else {
			out.Directories = append(out.Directories, value)
		}
	}
	return out, nil
}

// ParseSkills reads one skills value, `LOCATION[@REF][//PATH]` (ADR
// 26-10-15-664), with a relative directory or shorthand judged against base
// (the working directory when empty): a skills source, with shorthand
// expanded to its URL, or else dir, the directory made absolute, whose skills
// are read as content. A directory that is there wins whatever its name holds
// (sourceref.Ref.Resolve); one that is not takes no @REF and no //PATH, which
// name a commit and a directory of a Git repository. An error about a value
// that names a repository is about its PATH or REF, so the refusal names what
// was wrong rather than a directory nobody meant.
func ParseSkills(value, base string) (repo SkillsRepository, dir string, err error) {
	source, err := sourceref.Parse(value)
	if err != nil {
		return SkillsRepository{}, "", err
	}
	if source, err = source.Resolve(base); err != nil {
		return SkillsRepository{}, "", err
	}
	if source.Kind != sourceref.Git {
		if source.Ref != "" || source.Path != "" {
			return SkillsRepository{}, "", fmt.Errorf("%s is not a directory here, and a directory's skills are read as they are; @REF and //PATH name a commit and a directory of a Git repository", source.Location)
		}
		return SkillsRepository{}, source.Location, nil
	}
	skillsDir := sandboxconfig.RootSkillsDirectory
	if source.Path != "" {
		skillsDir, source.Path = source.Path, ""
	}
	if err := sandboxconfig.ValidateSkillsDirectory(skillsDir); err != nil {
		return SkillsRepository{}, "", err
	}
	return SkillsRepository{Arg: strings.TrimSpace(value), Source: source, Directory: skillsDir}, "", nil
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
	refs := map[string]SkillsRepository{}
	var skillsSources []apiclientgen.SandboxSkillsSource
	for _, repo := range repos {
		repository := repo.Source.RepositoryKey()
		key, resolved := keys[repo.Source.Key()]
		if !resolved {
			if other, ok := refs[repository]; ok {
				err := fmt.Errorf("%s and %s name one repository at two refs; a discobox checks out one", other, repo)
				if other.Project || repo.Project {
					err = fmt.Errorf("%w (--no-project-skills leaves the project's out)", err)
				}
				return err
			}
			reference, err := resolveNamedReference(ctx, repo.Source.String(), placement, sourceOptions, used)
			if err != nil {
				return fmt.Errorf("%s: %w", repo, err)
			}
			if occupied(reference.Key) {
				reference.Resolved.close()
				return fmt.Errorf("%s lands at %s, where another source already is", repo, reference.Key)
			}
			reference.APISource.SetPurpose(apiclientgen.NewOptGitSourcePurpose(apiclientgen.GitSourcePurposeSkills))
			local.add(reference.Key, reference.Resolved)
			references[reference.Key] = *reference.APISource
			key = reference.Key
			keys[repo.Source.Key()] = key
			refs[repository] = repo
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
