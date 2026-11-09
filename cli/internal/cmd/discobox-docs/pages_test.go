package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The public manual ships in the release it describes, and the documentation
// site labels each snapshot with its version and release status. A page that
// calls its own line a prerelease therefore reads wrong under a stable label,
// and a stable tag's import cannot edit the prose.
func TestPublicPagesNameNoReleaseStatus(t *testing.T) {
	// Describing the edge channel, which does carry prereleases, is not a
	// statement about the manual's own line.
	allowed := []string{"The edge installer includes release candidates and other prereleases."}
	prerelease := regexp.MustCompile(`(?i)pre-?release`)
	pages, err := filepath.Glob(filepath.Join("..", "..", "..", "..", "docs", "public", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatal("no public pages found")
	}
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		// Prose wraps, so compare with line breaks folded to spaces.
		text := strings.Join(strings.Fields(string(body)), " ")
		for _, sentence := range allowed {
			text = strings.ReplaceAll(text, sentence, "")
		}
		if found := prerelease.FindString(text); found != "" {
			t.Errorf("%s says %q; the documentation site, not the page, shows a release's status", filepath.Base(page), found)
		}
	}
}

// The site imports exactly the pages docs/public-import.md allowlists and
// rejects a link it cannot resolve, so a page left off the list, or a link to
// one, fails the import.
func TestPublicPagesAreAllowlisted(t *testing.T) {
	docs := filepath.Join("..", "..", "..", "..", "docs")
	contract, err := os.ReadFile(filepath.Join(docs, "public-import.md"))
	if err != nil {
		t.Fatal(err)
	}
	allowlisted := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^\\| `docs/public/([^`]+\\.md)` \\|").FindAllStringSubmatch(string(contract), -1) {
		allowlisted[m[1]] = true
	}
	if len(allowlisted) == 0 {
		t.Fatal("no allowlisted pages found in public-import.md")
	}

	// Walk the whole tree: the import takes no assets and no subdirectories,
	// so anything that is not an allowlisted top-level page is an error.
	public := filepath.Join(docs, "public")
	var pages []string
	err = filepath.WalkDir(public, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(public, path)
		if err != nil {
			return err
		}
		if !allowlisted[filepath.ToSlash(rel)] {
			t.Errorf("docs/public/%s is not in public-import.md's allowlist, so the site never imports it", filepath.ToSlash(rel))
		}
		if filepath.Ext(path) == ".md" {
			pages = append(pages, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, page := range pages {
		present[filepath.Base(page)] = true
	}
	for name := range allowlisted {
		if !present[name] {
			t.Errorf("public-import.md allowlists docs/public/%s, which does not exist", name)
		}
	}

	// A relative link is any Markdown link target, inline (with or without a
	// title) or a reference definition, without a scheme or a leading '#'.
	// Only the page is checked, not the fragment.
	inline := regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	reference := regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:\s*<?([^\s>]+)`)
	for _, page := range pages {
		name := filepath.Base(page)
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		links := append(inline.FindAllStringSubmatch(string(body), -1), reference.FindAllStringSubmatch(string(body), -1)...)
		for _, m := range links {
			target := m[1]
			if strings.HasPrefix(target, "#") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			if !allowlisted[target] {
				t.Errorf("docs/public/%s links to %q, which is not an allowlisted page", name, m[1])
			}
		}
	}
}
