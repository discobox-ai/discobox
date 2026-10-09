package cli

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/github"
)

// Anyone who can comment on an issue writes its text, so none of it reaches
// the console's screen as something a terminal would act on.
func TestAnIssueIsMadeSafeToDraw(t *testing.T) {
	t.Parallel()
	evil := "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07\x1b[2J\u202e"
	issue := tuiIssue(github.Issue{
		Title:  "title " + evil,
		Body:   "line one\r\nline two " + evil,
		Labels: []github.Label{{Name: evil, Color: "ffffff"}},
		Timeline: []github.Event{
			{Kind: "commented", Actor: evil, Body: "comment\r\n" + evil},
			{Kind: "cross-referenced", Source: &github.Ref{Title: evil}},
		},
	})
	for name, value := range map[string]string{
		"title": issue.Title, "body": issue.Body, "label": issue.Labels[0].Name,
		"actor": issue.Timeline[0].Actor, "comment": issue.Timeline[0].Body,
		"reference": issue.Timeline[1].Source.Title,
	} {
		if strings.ContainsAny(value, "\x1b\x07\u202e\r") {
			t.Errorf("the %s reaches the screen raw: %q", name, value)
		}
	}
	if !strings.Contains(issue.Body, "line one\nline two") {
		t.Errorf("the body lost its lines: %q", issue.Body)
	}
}
