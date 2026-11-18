// Package hostskill owns the embedded skill and its host audience marker.
package hostskill

import (
	_ "embed"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pkg/version"
	"github.com/discobox-ai/x/frontmatter"
	"gopkg.in/yaml.v3"
)

const Name = "discobox-host"

//go:embed SKILL.md
var template string

func Content() string {
	return strings.Replace(template, `"CLI_VERSION"`, strconv.Quote(version.String()), 1)
}

// Directories returns the skill directories under a home or Git worktree root.
func Directories(root string) []string {
	return []string{filepath.Join(root, ".claude", "skills"), filepath.Join(root, ".agents", "skills")}
}

// Marked recognizes only front matter. Other skills may have arbitrary
// Markdown or front matter we cannot interpret.
func Marked(content []byte) bool {
	parsed, err := frontmatter.Parse(content)
	if err != nil || parsed.Delimiter != frontmatter.DelimiterPlain || parsed.HasShebang {
		return false
	}
	var header struct {
		Metadata struct {
			Discobox struct {
				Audience string
			}
		}
	}
	return yaml.Unmarshal(parsed.Meta, &header) == nil && header.Metadata.Discobox.Audience == "host"
}
