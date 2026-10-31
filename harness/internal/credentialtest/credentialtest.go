// Package credentialtest exercises a built-in's real credential schema.
package credentialtest

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
)

func Resolve(t *testing.T, path, content string, sentinels map[string]string) string {
	t.Helper()
	raw, err := os.ReadFile("image.json")
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Harness struct {
			SaveBack harnesscapture.Definition `json:"saveBack"`
		} `json:"harness"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	out, err := metadata.Harness.SaveBack.ResolveCredential(path, content, sentinels)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
