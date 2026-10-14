package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/clientconfig"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// listProjectSkillsSources is the project's skills sources, in the order they
// were added (ADR 26-10-14-894 §1). A server that predates them is a project
// with none: it answers a person with net/http's own plain-text 404, and a
// discobox with its sandbox role's refusal of a route the role does not name.
func listProjectSkillsSources(ctx context.Context, client *apiclientgen.Client, projectID string) ([]apimodel.ProjectSkillsSource, error) {
	res, err := client.ListProjectSkillsSources(ctx, apiclientgen.ListProjectSkillsSourcesParams{ProjectId: projectID})
	if err != nil {
		var plain *plainStatusError
		if errors.As(err, &plain) &&
			(plain.code == http.StatusNotFound && plain.body == "404 page not found" ||
				plain.code == http.StatusForbidden && plain.body == "not in the sandbox role") {
			return nil, nil
		}
		return nil, err
	}
	body, err := expectResponse[apimodel.ListProjectSkillsSourcesBody](res)
	if err != nil {
		return nil, err
	}
	return body.SkillsSources, nil
}

// projectSkills is what a create copies in ahead of its own --skills: the
// project's skills sources, each as the --skills value it stands for (ADR
// 26-10-14-894 §2). The client reads them because it is what pins and
// delivers a source; the server never adds them to a create.
func projectSkills(ctx context.Context, client *apiclientgen.Client, projectID string) ([]sandboxcreate.SkillsRepository, error) {
	sources, err := listProjectSkillsSources(ctx, client, projectID)
	if err != nil {
		return nil, fmt.Errorf("read the project's skills sources: %w", err)
	}
	out := make([]sandboxcreate.SkillsRepository, 0, len(sources))
	for _, source := range sources {
		repo, err := projectSkillsRepository(source)
		if err != nil {
			return nil, fmt.Errorf("%w; remove it with discobox skills rm %s, or leave the project's out with --no-project-skills", err, source.ID)
		}
		out = append(out, repo)
	}
	return out, nil
}

// projectSkillsRepository is one entry as the --skills value it stands for,
// and the refusal it earns (sandboxcreate.ProjectSkillsRepository).
func projectSkillsRepository(source apimodel.ProjectSkillsSource) (sandboxcreate.SkillsRepository, error) {
	return sandboxcreate.ProjectSkillsRepository(source.URL, source.Ref.Or(""), source.Directory)
}

// newSkillsCommand manages the skills every new discobox gets: the project's
// skills repositories on the server, and this client's new.skills in
// client.yaml. A value is filed by what it is, read as --skills reads it: a
// Git URL is the project's, a directory is this machine's (ADR 26-10-14-894
// §3).
func (a *App) newSkillsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage the skills every new discobox gets",
		Long: "Manage the skills every new discobox gets.\n\n" +
			"A Git URL[@REF][//PATH] is the project's: every discobox of the project is\n" +
			"created with it, whoever creates it, checked out under skill-sources with each\n" +
			"skill in PATH linked in. A directory is this machine's, kept in client.yaml's\n" +
			"new.skills, and copied into every discobox created from here.\n\n" +
			"A create adds the project's first, then client.yaml's or its own --skills, so\n" +
			"yours win on a skill of the same name. --no-project-skills leaves the\n" +
			"project's out of one create.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.listSkills(cmd) },
	}
	list := &cobra.Command{
		Use: "ls", Aliases: []string{"list"}, Short: "List the project's skills repositories and client.yaml's skills",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.listSkills(cmd) },
	}
	add := &cobra.Command{
		Use:   "add URL[@REF][//PATH]|DIR...",
		Short: "Add a skills repository to the project, or a directory to client.yaml",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runActionMany(cmd, args, "skills value", "add", "added", func(arg string) (string, error) {
				return a.addSkills(cmd.Context(), arg)
			})
		},
	}
	remove := &cobra.Command{
		Use:     "rm URL[@REF][//PATH]|ID|DIR...",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a skills repository from the project, or a directory from client.yaml",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runActionMany(cmd, args, "skills value", "remove", "removed", func(arg string) (string, error) {
				return a.removeSkills(cmd.Context(), arg)
			})
		},
	}
	cmd.AddCommand(list, add, remove)
	return cmd
}

// skillsListing is `discobox skills ls --output json`.
type skillsListing struct {
	Project []apimodel.ProjectSkillsSource `json:"project"`
	Client  clientSkills                   `json:"client"`
}

// clientSkills is client.yaml's new.skills, as a create reads them, and the
// file they are in: empty when none is read.
type clientSkills struct {
	Path   string   `json:"path,omitempty"`
	Skills []string `json:"skills"`
}

func (a *App) listSkills(cmd *cobra.Command) error {
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	project, err := listProjectSkillsSources(cmd.Context(), client, projectID)
	if err != nil {
		return err
	}
	cfg, err := loadClientConfig()
	if err != nil {
		return err
	}
	listing := skillsListing{Project: project, Client: clientSkills{Path: cfg.Path, Skills: cfg.New.Skills}}
	if listing.Project == nil {
		listing.Project = []apimodel.ProjectSkillsSource{}
	}
	if listing.Client.Skills == nil {
		listing.Client.Skills = []string{}
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), listing)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FROM\tSKILLS\tID")
	for _, source := range project {
		repo, _ := projectSkillsRepository(source)
		fmt.Fprintf(tw, "project\t%s\t%s\n", repo.Arg, source.ID)
	}
	for _, skill := range cfg.New.Skills {
		fmt.Fprintf(tw, "client.yaml\t%s\t\n", skill)
	}
	return tw.Flush()
}

// projectClient is the server client and project an edit of the project's
// skills sources goes to, asked for only when a value is the project's: a
// directory is this machine's, and adding one reaches no server.
type projectClient func() (*apiclientgen.Client, string, error)

func (a *App) projectClient() (*apiclientgen.Client, string, error) {
	client, err := a.apiClient()
	if err != nil {
		return nil, "", err
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return nil, "", err
	}
	return client, projectID, nil
}

func (a *App) addSkills(ctx context.Context, arg string) (string, error) {
	added, err := addSkills(ctx, a.projectClient, arg)
	if err != nil {
		return "", err
	}
	if added.Project {
		return added.ID, nil
	}
	return added.Value + " (" + added.File + ")", nil
}

func (a *App) removeSkills(ctx context.Context, arg string) (string, error) {
	removed, err := removeSkills(ctx, a.projectClient, arg)
	if err != nil {
		return "", err
	}
	if removed.Project {
		return removed.ID, nil
	}
	return removed.Value + " (" + removed.File + ")", nil
}

// skillsEdit is what one add or remove did: a project entry, by its ID, or a
// directory of client.yaml's, and the file.
type skillsEdit struct {
	Project bool
	ID      string
	Value   string
	File    string
}

// addSkills files one value where it belongs, read as --skills reads it: a
// Git URL in the project's skills sources, a directory in client.yaml's
// new.skills (ADR 26-10-14-894 §3).
func addSkills(ctx context.Context, project projectClient, arg string) (skillsEdit, error) {
	split, err := sandboxcreate.SplitSkills([]string{arg})
	if err != nil {
		return skillsEdit{}, err
	}
	if len(split.Repositories) == 1 {
		repo := split.Repositories[0]
		client, projectID, err := project()
		if err != nil {
			return skillsEdit{}, err
		}
		url, ref := repo.URLAndRef()
		// The server holds an entry to the same rule; asked here first, a
		// refusal reads as itself rather than as a request that failed.
		if err := sandboxconfig.ValidateSkillsRepository(url, ref); err != nil {
			return skillsEdit{}, err
		}
		body := &apimodel.CreateProjectSkillsSourceBody{URL: url}
		if ref != "" {
			body.SetRef(apiclientgen.NewOptString(ref))
		}
		body.SetDirectory(apiclientgen.NewOptString(repo.Directory))
		res, err := client.CreateProjectSkillsSource(ctx, body, apiclientgen.CreateProjectSkillsSourceParams{ProjectId: projectID})
		if err != nil {
			return skillsEdit{}, err
		}
		source, err := expectResponse[apimodel.ProjectSkillsSource](res)
		if err != nil {
			return skillsEdit{}, err
		}
		added, _ := projectSkillsRepository(*source)
		return skillsEdit{Project: true, ID: source.ID, Value: added.Arg}, nil
	}
	dir, err := skillsDirectory(arg)
	if err != nil {
		return skillsEdit{}, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return skillsEdit{}, err
	}
	if !info.IsDir() {
		return skillsEdit{}, fmt.Errorf("%s is not a directory of skills", dir)
	}
	path, err := clientconfig.AddSkill(dir)
	if err != nil {
		return skillsEdit{}, err
	}
	return skillsEdit{Value: dir, File: path}, nil
}

// removeSkills takes one value out of where it is. client.yaml is asked
// first, for the value as a create reads it, since an entry there may be a
// Git URL or a directory of any name; anything it does not name is the
// project's, by its ID or a unique prefix of one, or by the URL[@REF][//PATH]
// it stands for.
func removeSkills(ctx context.Context, project projectClient, arg string) (skillsEdit, error) {
	arg = strings.TrimSpace(arg)
	value, err := skillsDirectory(arg)
	if err != nil {
		return skillsEdit{}, err
	}
	cfg, err := loadClientConfig()
	if err != nil {
		return skillsEdit{}, err
	}
	if slices.Contains(cfg.New.Skills, value) {
		return removeClientSkill(value)
	}
	client, projectID, err := project()
	if err != nil {
		return skillsEdit{}, err
	}
	sources, err := listProjectSkillsSources(ctx, client, projectID)
	if err != nil {
		return skillsEdit{}, err
	}
	source, ok := matchProjectSkillsSource(sources, arg)
	if !ok {
		return skillsEdit{}, fmt.Errorf("neither the project nor %s names %s", clientConfigName(cfg.Path), arg)
	}
	return removeProjectSkills(ctx, client, projectID, source)
}

// removeProjectSkills deletes one project entry.
func removeProjectSkills(ctx context.Context, client *apiclientgen.Client, projectID string, source apimodel.ProjectSkillsSource) (skillsEdit, error) {
	res, err := client.DeleteProjectSkillsSource(ctx, apiclientgen.DeleteProjectSkillsSourceParams{ProjectId: projectID, SkillsSourceId: source.ID})
	if err != nil {
		return skillsEdit{}, err
	}
	if err := expectNoContent[apiclientgen.DeleteProjectSkillsSourceNoContent](res); err != nil {
		return skillsEdit{}, err
	}
	repo, _ := projectSkillsRepository(source)
	return skillsEdit{Project: true, ID: source.ID, Value: repo.Arg}, nil
}

// removeClientSkill takes one entry, as a create reads it, out of
// client.yaml's new.skills.
func removeClientSkill(value string) (skillsEdit, error) {
	path, err := clientconfig.RemoveSkill(value)
	if err != nil {
		return skillsEdit{}, err
	}
	return skillsEdit{Value: value, File: path}, nil
}

func clientConfigName(path string) string {
	if path == "" {
		return "client.yaml"
	}
	return path
}

// matchProjectSkillsSource is the one entry arg names: by its ID or a unique
// prefix of it, or by the URL[@REF][//PATH] it stands for, read as --skills
// reads it.
func matchProjectSkillsSource(sources []apimodel.ProjectSkillsSource, arg string) (apimodel.ProjectSkillsSource, bool) {
	var byPrefix []apimodel.ProjectSkillsSource
	for _, source := range sources {
		if source.ID == arg {
			return source, true
		}
		if strings.HasPrefix(source.ID, arg) {
			byPrefix = append(byPrefix, source)
		}
	}
	if len(byPrefix) == 1 {
		return byPrefix[0], true
	}
	if split, err := sandboxcreate.SplitSkills([]string{arg}); err == nil && len(split.Repositories) == 1 {
		want := split.Repositories[0]
		for _, source := range sources {
			if have, _ := projectSkillsRepository(source); have.Source == want.Source && have.Directory == want.Directory {
				return source, true
			}
		}
	}
	return apimodel.ProjectSkillsSource{}, false
}

// skillsDirectory is a value named on the command line as a create reads it:
// a Git URL as given, and a directory absolute, since it is written into
// client.yaml, where a relative path would be relative to the file rather
// than to where it was typed.
func skillsDirectory(arg string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return clientconfig.ResolveSkill(wd, arg)
}
