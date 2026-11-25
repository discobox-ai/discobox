package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeADRs(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadReadsEverySpellingInIDOrder(t *testing.T) {
	dir := writeADRs(t, map[string]string{
		"README.md": "# Architecture Decision Records\n",
		"26-11-02-001-later.md": "# 26-11-02-001 — Later | piped\n\n" +
			"- **Status**: Accepted (supersedes\n  [0002](0002-old.md) §1)\n- **Date**: 2026-11-02\n\n## Context\n",
		"26-10-01-002-earlier.md": "# 26-10-01-002 — Earlier\n\nStatus: Proposed\nDate: 2026-10-01\n",
		"0002-old.md":             "# 0002. Old\n\n**Status:** Superseded in part\n",
	})
	adrs, problems, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	want := []adr{
		{ID: "0002", File: "0002-old.md", Title: "Old", Status: "Superseded in part"},
		{ID: "26-10-01-002", File: "26-10-01-002-earlier.md", Title: "Earlier", Status: "Proposed"},
		{ID: "26-11-02-001", File: "26-11-02-001-later.md", Title: "Later | piped", Status: "Accepted (supersedes [0002](0002-old.md) §1)"},
	}
	if len(adrs) != len(want) {
		t.Fatalf("got %d ADRs, want %d: %+v", len(adrs), len(want), adrs)
	}
	for i := range want {
		if adrs[i] != want[i] {
			t.Errorf("ADR %d = %+v, want %+v", i, adrs[i], want[i])
		}
	}

	var b strings.Builder
	if err := write(&b, filter(adrs, "proposed")); err != nil {
		t.Fatal(err)
	}
	if got, wantTable := b.String(), "| ADR | Title | Status |\n| --- | --- | --- |\n"+
		"| [26-10-01-002](26-10-01-002-earlier.md) | Earlier | Proposed |\n"; got != wantTable {
		t.Errorf("table =\n%s\nwant\n%s", got, wantTable)
	}
	b.Reset()
	if err := write(&b, adrs[2:]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `| Later \| piped |`) {
		t.Errorf("a pipe in a title is not escaped:\n%s", b.String())
	}
}

func TestLoadNamesWhatTheIndexCannotRead(t *testing.T) {
	dir := writeADRs(t, map[string]string{
		"26-11-02-001-no-status.md":     "# 26-11-02-001 — No status\n\n## Context\n\nStatus: Accepted\n",
		"26-11-02-002-bad-heading.md":   "# 26-11-02-002: Bad heading\n\n- **Status**: Proposed\n",
		"26-11-02-003-no-heading.md":    "- **Status**: Proposed\n",
		"not-an-adr.md":                 "# Notes\n",
		"26-11-02-004-fine.md":          "# 26-11-02-004 — Fine\n\n- **Status**: Proposed\n",
		"0001-four-digit-heading-ok.md": "# 0001 — Four\n\nStatus: Accepted\n",
	})
	adrs, problems, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(adrs) != 5 {
		t.Errorf("got %d ADRs, want 5 (every file named by an ID): %+v", len(adrs), adrs)
	}
	for _, want := range []string{
		"26-11-02-001-no-status.md: no `- **Status**: …` line",
		"26-11-02-002-bad-heading.md: the heading is not `# 26-11-02-002 — <Title>`",
		"26-11-02-003-no-heading.md: no `# <ID> — <Title>` heading",
		"not-an-adr.md: the file name does not start with an ADR ID",
	} {
		found := false
		for _, p := range problems {
			if strings.Contains(p, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no problem %q in %q", want, problems)
		}
	}
	if len(problems) != 4 {
		t.Errorf("got %d problems, want 4: %q", len(problems), problems)
	}
}

// TestRepositoryADRsAreReadable is the check CI runs, against this
// repository's own ADRs.
func TestRepositoryADRsAreReadable(t *testing.T) {
	_, problems, err := load(filepath.Join("..", "..", "..", "docs", "adr"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
