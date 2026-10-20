package targets

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"

	"github.com/discobox-ai/discobox/api/openapi"
	"github.com/discobox-ai/discobox/server/internal/targetdb"
)

// gitOverHTTPS is the prose of how git clones, fetches and pushes on
// github.com: there is no machine-readable description of it.
//
//go:embed builtin/github-git.md
var gitOverHTTPS []byte

// builtin is an entry that ships with Discobox, and the text of each of its
// documents the binary holds. A document with a URL is fetched instead.
type builtin struct {
	entry targetdb.Entry
	texts map[string][]byte
}

// The pinned sources of GitHub's documents. A URL names a commit, never a
// branch, so what it answers is what the digest says; moving one is a change
// to the built-in entry, and to its version.
const (
	githubRESTURL    = "https://raw.githubusercontent.com/github/rest-api-description/755ee33b4a81d51f6f07a902cce26cbec3b822f2/descriptions/api.github.com/api.github.com.json"
	githubRESTSHA256 = "3177ad6a86ca04c57a9bef3fc24e28a339efb8eb040c6f950af3d3ed2fee7fa3"
	// docs.github.com serves the same schema at /public/fpt/schema.docs.graphql,
	// but there it moves with every release; the docs repository pins it.
	githubGraphQLURL    = "https://raw.githubusercontent.com/github/docs/44a609d0382839fdfe455de859774cfebb7f2d83/src/graphql/data/fpt/schema.docs.graphql"
	githubGraphQLSHA256 = "ee690896ae7d9126765c6a660174b40c9d1974b70db36af8559117b9c4a7b7a6"
)

// discoboxAPIHost is the host a discobox reaches the discobox API at, through
// its pool (wellknown.DiscoboxSandbox, ADR 0140).
const discoboxAPIHost = "api.discobox.internal"

// builtins are the entries Discobox ships (ADR 26-10-20-782 §6): GitHub, and
// the discobox API itself. Each holds documents alone: neither has terms or
// statement shapes yet.
func builtins() []builtin {
	return []builtin{
		{
			entry: targetdb.Entry{
				Host: "github.com",
				Documents: []targetdb.Document{
					{Name: "rest", Kind: targetdb.OpenAPI, URL: githubRESTURL, SHA256: githubRESTSHA256},
					{Name: "graphql", Kind: targetdb.GraphQL, URL: githubGraphQLURL, SHA256: githubGraphQLSHA256},
					{Name: "git", Kind: targetdb.Prose, SHA256: digest(gitOverHTTPS)},
				},
			},
			texts: map[string][]byte{"git": gitOverHTTPS},
		},
		{
			entry: targetdb.Entry{
				Host: discoboxAPIHost,
				Documents: []targetdb.Document{
					{Name: "api", Kind: targetdb.OpenAPI, SHA256: digest(openapi.ServerYAML)},
				},
			},
			texts: map[string][]byte{"api": openapi.ServerYAML},
		},
	}
}

func digest(text []byte) string {
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:])
}

// checkBuiltins reports a built-in entry that is not one, or a document of
// one with neither a URL nor text.
func checkBuiltins(entries []builtin) error {
	for _, b := range entries {
		if err := b.entry.Validate(); err != nil {
			return fmt.Errorf("built-in target entry %s: %w", b.entry.Host, err)
		}
		for _, d := range b.entry.Documents {
			if _, ok := b.texts[d.Name]; ok == (d.URL != "") {
				return fmt.Errorf("built-in target entry %s: document %s needs a URL or text, not both", b.entry.Host, d.Name)
			}
		}
	}
	return nil
}
