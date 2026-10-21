package access

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/policy"
)

// runFacts shows what the service recorded about this discobox and what each
// variable a policy may use resolves to for it (ADR 26-10-20-782 §4), so an
// agent writing a policy writes it against the repository it is working on
// and knows what $primary.repo will mean when a person approves it. The
// values are the service's record, never this discobox's git config.
func runFacts(ctx context.Context, args []string) int {
	var structured bool
	flags := flag.NewFlagSet(Name+" facts", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "emit JSON")
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)
	if len(flags.Args()) > 0 {
		return usageError(out, "unexpected argument %q", flags.Arg(0))
	}

	facts, err := newClient().Facts(ctx)
	if errors.Is(err, agentcreds.ErrNotFound) {
		// A service from before the verb answers 404 for its path, but so does
		// one that does not know this discobox, and the two cannot be told
		// apart here: the service's own message is kept, and both causes said.
		return out.report(fmt.Errorf("%w: %s (a credentials service that predates %s facts answers the same way)", agentcreds.ErrNotFound, agentcreds.Message(err), Name))
	}
	if err != nil {
		return out.report(err)
	}
	if facts.Variables == nil {
		facts.Variables = []policy.Variable{}
	}
	out.emit(facts, func(w io.Writer) { writeFacts(w, facts) })
	return exitOK
}

// writeFacts is the whole record: the discobox, its sources, and every
// variable with its value or the fact that it has none.
func writeFacts(w io.Writer, facts agentcreds.FactsResponse) {
	discobox := facts.Facts.Discobox
	fmt.Fprintf(w, "This discobox is %s", cmp.Or(discobox.ID, "unnamed"))
	if discobox.Project != "" {
		fmt.Fprintf(w, ", in project %s", discobox.Project)
	}
	// No creator is recorded both when a person created it and when the
	// service records nothing, so only a recorded one is said.
	if discobox.CreatedBy != "" {
		fmt.Fprintf(w, ", created by discobox %s", discobox.CreatedBy)
	}
	fmt.Fprintln(w, ".")
	fmt.Fprintln(w, primarySentence(facts.Facts))
	var others []policy.Source
	for _, source := range facts.Facts.Sources {
		if !source.Primary {
			others = append(others, source)
		}
	}
	if len(others) > 0 {
		fmt.Fprintln(w, "Its other sources:")
		for _, source := range others {
			fmt.Fprintf(w, "  %s: %s\n", cmp.Or(source.Slug, "(no slug)"), describeSource(source))
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Variables a policy may use, as they resolve for this discobox:")
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, variable := range facts.Variables {
		value := variable.Value
		if !variable.Resolved {
			value = "(resolves to nothing, so matches nothing)"
		}
		fmt.Fprintf(table, "  %s\t%s\n", variable.Name, value)
	}
	_ = table.Flush()
	fmt.Fprintln(w)
	fmt.Fprintln(w, "These come from the credentials service's record of this discobox, never")
	fmt.Fprintln(w, "from its git config, and are what a policy's variables resolve to when a")
	fmt.Fprintln(w, "person approves it.")
}

// primarySentence says what this discobox is working on and how a policy
// names it, in one line: what list leads with.
func primarySentence(facts policy.Facts) string {
	var primary *policy.Source
	for i := range facts.Sources {
		if facts.Sources[i].Primary {
			primary = &facts.Sources[i]
		}
	}
	switch {
	case primary == nil:
		return "This discobox has no primary source: the $primary variables resolve to nothing, and match nothing."
	case primary.Remote == nil:
		return "This discobox's primary source has no network remote: $primary.repo and $primary.host resolve to nothing, and match nothing."
	}
	return fmt.Sprintf("This discobox's primary source is %s; write %s for %s.", describeSource(*primary), policy.VarPrimaryRepo, primary.Remote.Repo)
}

// describeSource is a source as it was created: where, at what, and when its
// remote is none, that.
func describeSource(source policy.Source) string {
	var b strings.Builder
	if source.Remote != nil {
		b.WriteString(source.Remote.Host + "/" + source.Remote.Repo)
	} else {
		b.WriteString("no network remote")
	}
	if source.Ref != nil {
		b.WriteString(" at ")
		if source.Ref.Type != "" {
			b.WriteString(source.Ref.Type + " ")
		}
		b.WriteString(source.Ref.Name)
	}
	if source.Commit != "" {
		fmt.Fprintf(&b, " (commit %s)", shortCommit(source.Commit))
	}
	return b.String()
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
