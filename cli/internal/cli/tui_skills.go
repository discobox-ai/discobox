package cli

import (
	"context"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// Skills is the skills screen's listing: the project's skills sources on the
// server named, then client.yaml's new.skills, which are this machine's
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
	for _, source := range sources {
		listing.Entries = append(listing.Entries, tui.SkillsEntry{
			Project: true,
			ID:      source.ID,
			Value:   projectSkillsRepository(source).Arg,
			Added:   source.CreatedAt,
		})
	}
	for _, skill := range cfg.New.Skills {
		listing.Entries = append(listing.Entries, tui.SkillsEntry{Value: skill})
	}
	return listing, nil
}

func (d *apiDataSource) AddSkills(ctx context.Context, server, value string) (tui.SkillsEntry, error) {
	added, err := addSkills(ctx, d.projectClient(ctx, server), value)
	if err != nil {
		return tui.SkillsEntry{}, err
	}
	return tui.SkillsEntry{Project: added.Project, ID: added.ID, Value: added.Value}, nil
}

// RemoveSkills removes a project entry by its ID, so a second entry of the
// same repository is never the one taken, and a directory by its path.
func (d *apiDataSource) RemoveSkills(ctx context.Context, server string, entry tui.SkillsEntry) error {
	value := entry.Value
	if entry.Project {
		value = entry.ID
	}
	_, err := removeSkills(ctx, d.projectClient(ctx, server), value)
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
