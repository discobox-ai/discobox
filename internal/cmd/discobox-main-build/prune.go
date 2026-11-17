package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/version"
)

// client is just enough of the GitHub REST API for pruning.
type client struct {
	api   string
	token string
	http  *http.Client
}

func newClient(api, token string) *client {
	return &client{api: strings.TrimRight(api, "/"), token: token, http: &http.Client{Timeout: time.Minute}}
}

func (c *client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// pages fetches every page of a list, a hundred at a time.
func pages[T any](ctx context.Context, c *client, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var all []T
	for page := 1; ; page++ {
		var batch []T
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page), &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

type githubRelease struct {
	ID        int64     `json:"id"`
	TagName   string    `json:"tag_name"`
	CreatedAt time.Time `json:"created_at"`
	// Draft is a build whose upload never finished: release:publish makes
	// one a draft until its assets are up. GitHub creates no tag for it.
	Draft bool `json:"draft"`
}

type githubPackage struct {
	Name string `json:"name"`
}

type packageVersion struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Metadata  struct {
		Container struct {
			Tags []string `json:"tags"`
		} `json:"container"`
	} `json:"metadata"`
}

type pruner struct {
	releases, packages *client
	repo, owner        string
	cutoff             time.Time
	dryRun             bool
	log                io.Writer
}

// prune deletes the main builds older than the cutoff, the newest published
// one excepted, and then the image versions that belonged only to main builds
// that are gone. A draft is a publish that failed partway and is never what
// the channel serves, so it is never the one kept, and it has no tag to delete.
// Releases first, so a build whose images could not be deleted is no longer
// installable, and the next run retries its images on their own.
func (p pruner) prune(ctx context.Context) error {
	releases, err := pages[githubRelease](ctx, p.releases, "/repos/"+p.repo+"/releases")
	if err != nil {
		return err
	}
	var builds []githubRelease
	for _, r := range releases {
		if version.MainBuild(r.TagName) {
			builds = append(builds, r)
		}
	}
	sort.Slice(builds, func(i, j int) bool { return builds[i].CreatedAt.After(builds[j].CreatedAt) })
	kept := map[string]bool{}
	newest := true
	for _, r := range builds {
		if !r.Draft && newest {
			newest = false
			kept[r.TagName] = true
			continue
		}
		if !r.CreatedAt.Before(p.cutoff) {
			kept[r.TagName] = true
			continue
		}
		fmt.Fprintf(p.log, "deleting main build %s (%s)\n", r.TagName, r.CreatedAt.Format(time.DateOnly))
		if p.dryRun {
			continue
		}
		if err := p.releases.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/releases/%d", p.repo, r.ID), nil); err != nil {
			return err
		}
		if r.Draft {
			continue
		}
		if err := p.releases.do(ctx, http.MethodDelete, "/repos/"+p.repo+"/git/refs/tags/"+url.PathEscape(r.TagName), nil); err != nil {
			return err
		}
	}
	fmt.Fprintf(p.log, "kept %d main build(s)\n", len(kept))
	return p.pruneImages(ctx, kept)
}

// pruneImages deletes each discobox-* image version older than the cutoff
// whose every tag is a main build's that is no longer kept. A version that
// also carries a release's tag, or :latest, is never one.
//
// An untagged version, an index's per-platform or attestation manifest, is
// never deleted, even once the index that named it is gone. The bundles are
// reproducible (fixed file times, unversioned binaries), so one platform's
// manifest is routinely the same bytes, and so the same ghcr version, in many
// builds and releases: deleting it with one index breaks every other that
// names it, and knowing none does means reading every remaining index on
// every run. What is left behind is manifests, not layers.
func (p pruner) pruneImages(ctx context.Context, kept map[string]bool) error {
	base, err := p.packagesBase(ctx)
	if err != nil {
		return err
	}
	pkgs, err := pages[githubPackage](ctx, p.packages, base+"?package_type=container")
	if err != nil {
		return err
	}
	for _, pkg := range pkgs {
		if !strings.HasPrefix(pkg.Name, "discobox-") {
			continue
		}
		path := base + "/container/" + url.PathEscape(pkg.Name) + "/versions"
		versions, err := pages[packageVersion](ctx, p.packages, path)
		if err != nil {
			return err
		}
		for _, v := range versions {
			tags := v.Metadata.Container.Tags
			if len(tags) == 0 || !v.CreatedAt.Before(p.cutoff) {
				continue
			}
			prunable := true
			for _, tag := range tags {
				if !version.MainBuild(tag) || kept[tag] {
					prunable = false
					break
				}
			}
			if !prunable {
				continue
			}
			fmt.Fprintf(p.log, "deleting %s:%s\n", pkg.Name, strings.Join(tags, ","))
			if p.dryRun {
				continue
			}
			if err := p.packages.do(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", path, v.ID), nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// packagesBase is where the owner's packages are listed: an organization's
// and a user's live under different paths.
func (p pruner) packagesBase(ctx context.Context) (string, error) {
	var owner struct {
		Type string `json:"type"`
	}
	if err := p.packages.do(ctx, http.MethodGet, "/users/"+url.PathEscape(p.owner), &owner); err != nil {
		return "", err
	}
	if owner.Type == "Organization" {
		return "/orgs/" + url.PathEscape(p.owner) + "/packages", nil
	}
	return "/users/" + url.PathEscape(p.owner) + "/packages", nil
}
