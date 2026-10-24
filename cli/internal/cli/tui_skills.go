package cli

import (
	"context"
	"os"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/sourceref"
)

// Skills is the skills screen's listing: the project's skills sources on the
// server named, then client.yaml's run.skills, which are this machine's
// whichever server the window is on (ADR 26-10-14-894 §3).
func (d *apiDataSource) Skills(ctx context.Context, server string) (tui.SkillsListing, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return tui.SkillsListing{}, err
	}
	sources, err := listProjectSkillsSources(ctx, d.client, d.projectID)
	if err != nil {
		return tui.SkillsListing{}, err
	}
	cfg, err := loadClientConfig()
	if err != nil {
		return tui.SkillsListing{}, err
	}
	listing := tui.SkillsListing{ClientFile: cfg.Path}
	home, _ := os.UserHomeDir()
	for _, source := range sources {
		repo, _ := projectSkillsRepository(source)
		listing.Entries = append(listing.Entries, tui.SkillsEntry{
			Project: true,
			ID:      source.ID,
			Value:   repo.Value(),
			Label:   repo.Display(home),
			Added:   source.CreatedAt,
		})
	}
	for _, skill := range cfg.Run.Skills {
		listing.Entries = append(listing.Entries, tui.SkillsEntry{Value: skill, Label: sourceref.Display(skill, home)})
	}
	return listing, nil
}

func (d *apiDataSource) AddSkills(ctx context.Context, server, value string) (tui.SkillsEntry, error) {
	added, err := addSkills(ctx, d.projectClient(ctx, server), value)
	if err != nil {
		return tui.SkillsEntry{}, err
	}
	home, _ := os.UserHomeDir()
	return tui.SkillsEntry{Project: added.Project, ID: added.ID, Value: added.Value, Label: sourceref.Display(added.Value, home)}, nil
}

// RemoveSkills removes the row where the screen says it is kept: a project
// entry by its ID, so a second entry of the same repository is never the one
// taken, and a client.yaml entry, Git URL or directory, by its value.
func (d *apiDataSource) RemoveSkills(ctx context.Context, server string, entry tui.SkillsEntry) error {
	if !entry.Project {
		_, err := removeClientSkill(entry.Value)
		return err
	}
	on, err := d.on(ctx, server)
	if err != nil {
		return err
	}
	_, err = removeProjectSkills(ctx, on.client, on.projectID, apimodel.ProjectSkillsSource{ID: entry.ID, URL: entry.Value, Directory: "."})
	return err
}

// projectClient is the client and project of the server named, for an edit of
// its project's skills sources.
func (d *apiDataSource) projectClient(ctx context.Context, server string) projectClient {
	return func() (*apiclientgen.Client, string, error) {
		on, err := d.on(ctx, server)
		if err != nil {
			return nil, "", err
		}
		return on.client, on.projectID, nil
	}
}
