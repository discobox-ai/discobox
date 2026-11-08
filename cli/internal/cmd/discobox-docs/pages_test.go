package main

import (
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
