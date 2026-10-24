package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/apipolicy"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/pkg/policy"
	"github.com/discobox-ai/discobox/pkg/wellknown"
)

// Approval rules (ADR 26-10-12-330): a project's standing answers to
// credential requests. A rule approves or says nothing — what it does not
// cover waits for a person — and only a person makes one.

func (a *App) newSecretRuleCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rule",
		Aliases: []string{"rules"},
		Short:   "Manage approval rules: standing answers to credential requests",
		Long: `An approval rule says a credential request like this, from a discobox of
these repositories, is approved without asking. Its repositories, credential,
hosts and purpose are facts the server checks: a rule answers a discobox whose
primary source is one of its repositories, compared exactly, or every discobox
in the project when it names none. Its policy is what it approves: a request
is covered when the policy it asks for falls within the rule's, compared by
code, with the judge reading only conditions code cannot compare. The rule's
variables, such as $primary.repo, bind to whichever discobox asks.

A rule answers only a request that asks for a policy; one that asks with uses
alone waits for a person each time.

A rule approves or says nothing. A request no rule covers waits for a person,
as it does with no rules, and "discobox secret request get" says why each rule
did not cover it. A grant a rule minted is independent of it: disabling or
deleting the rule revokes nothing, and "discobox secret grant ls --rule" lists
what it minted.

A rule is also made by answering a request with
"discobox secret request approve --auto".`,
	}
	cmd.AddCommand(a.newSecretRuleListCommand())
	cmd.AddCommand(a.newSecretRuleGetCommand())
	cmd.AddCommand(a.newSecretRuleCreateCommand())
	cmd.AddCommand(a.newSecretRuleUpdateCommand())
	cmd.AddCommand(a.newSecretRuleDeleteCommand())
	return cmd
}

func (a *App) newSecretRuleListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List approval rules", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		rules, err := listApprovalRules(cmd.Context(), client, projectID)
		if err != nil {
			return err
		}
		return a.writeApprovalRules(cmd, rules)
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSecretRuleGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get RULE_ID", Short: "Show an approval rule", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		ruleID, err := a.resolveApprovalRuleID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		res, err := client.GetApprovalRule(cmd.Context(), apiclientgen.GetApprovalRuleParams{ProjectId: projectID, RuleId: ruleID})
		if err != nil {
			return err
		}
		rule, err := expectResponse[apimodel.ApprovalRule](res)
		if err != nil {
			return err
		}
		return a.writeApprovalRule(cmd, rule)
	}}
}

func (a *App) newSecretRuleCreateCommand() *cobra.Command {
	var wellKnown, secretRef string
	maxTTL, expiresIn := lifetime.NewFlag(), lifetime.NewLapsingFlag()
	var statements, hosts, repositories []string
	definitions := policy.Definitions{}
	var disabled, everyDiscobox bool
	cmd := &cobra.Command{
		Use:   "create (--well-known ID | --secret SECRET) (--repo REPOSITORY... | --every-discobox) --statement SENTENCE... [--define WORD=DEFINITION...]",
		Short: "Create an approval rule",
		Long: `Creates an approval rule directly, as "discobox secret grant create" makes a
pre-approval. The rule is then tested against every request pending in the
project.

A rule says which discoboxes it is for. --repo names a repository, as a URL
or as host/path (github.com/org/repo): the rule answers a discobox whose
primary source is that repository, however its remote is spelled. A discobox
started from a local directory is of the remote its checkout tracks, and of
none when it tracks none. --every-discobox makes the rule for every discobox
in the project instead: it approves for discoboxes that do not exist yet,
including ones a discobox creates, and is never the default.

What it approves is a policy, written as discobox-access writes one: a
--statement for each sentence, and a --define for each word a statement uses
that is not a default (read, create, update, delete, manage, anything,
anywhere):

  --statement 'allow read on issue in repository $primary.repo' \
  --define 'issue=a GitHub issue' --define 'repository=a GitHub repository'

A variable such as $primary.repo is bound to whichever discobox asks. The
policy is parsed and checked before it is sent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			written, err := writtenPolicy(statements, definitions, "--statement", "--define")
			if err != nil {
				return err
			}
			if written == nil {
				return errors.New("a rule says what may be approved: give its policy, at least one --statement")
			}
			body := &apimodel.CreateApprovalRuleBody{Policy: *written, Hosts: grantHosts(hosts)}
			if body.Repositories, err = ruleRepositories(repositories, everyDiscobox, "--repo", "--every-discobox"); err != nil {
				return err
			}
			switch {
			case everyDiscobox:
				// Said, not left out: the server never reads a rule that
				// names no repository as one for every discobox.
				body.SetEveryDiscobox(apiclientgen.NewOptBool(true))
			case len(body.Repositories) == 0:
				return errors.New("a rule says which discoboxes it is for: --repo github.com/org/repo for the discoboxes of a repository, or --every-discobox for every discobox in the project")
			}
			switch wellKnown, secretRef = strings.TrimSpace(wellKnown), strings.TrimSpace(secretRef); {
			case (wellKnown == "") == (secretRef == ""):
				return errors.New("a rule names one credential: --well-known ID, or --secret SECRET")
			case wellKnown != "":
				body.SetWellKnownId(apiclientgen.NewOptString(wellKnown))
			default:
				secretID, err := a.resolveSecretID(cmd.Context(), client, projectID, secretRef)
				if err != nil {
					return err
				}
				body.SetSecretId(apiclientgen.NewOptString(secretID))
			}
			if seconds, given := maxTTL.Seconds(); given {
				body.SetMaxGrantTTLSeconds(apiclientgen.NewOptInt64(seconds))
			}
			if d, lapses := expiresIn.Duration(); lapses {
				body.SetExpiresAt(apiclientgen.NewOptDateTime(time.Now().Add(d).UTC()))
			}
			if disabled {
				body.SetEnabled(apiclientgen.NewOptBool(false))
			}
			res, err := client.CreateApprovalRule(cmd.Context(), body, apiclientgen.CreateApprovalRuleParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			rule, err := expectResponse[apimodel.ApprovalRule](res)
			if err != nil {
				return err
			}
			return a.writeApprovalRule(cmd, rule)
		},
	}
	cmd.Flags().StringVar(&wellKnown, "well-known", "", "The well-known credential the rule approves, by ID (e.g. com.github.api)")
	cmd.Flags().StringVar(&secretRef, "secret", "", "The project secret the rule approves, by name or ID; it answers an ask that names no well-known credential by the host it is bound to")
	cmd.Flags().StringSliceVar(&repositories, "repo", nil, "A repository the rule is for, as a URL or host/path (github.com/org/repo), comma-separated or repeated: it answers a discobox whose primary source is one of them")
	cmd.Flags().BoolVar(&everyDiscobox, "every-discobox", false, "The rule is for every discobox in the project. It approves for discoboxes that do not exist yet, including ones a discobox creates")
	cmd.Flags().StringArrayVar(&statements, "statement", nil, "A statement of the policy the rule approves, as a sentence (repeatable): a request is covered when its policy falls within this one")
	cmd.Flags().Var(definitions, "define", "WORD=DEFINITION for a word a statement uses that is not a default (repeatable)")
	cmd.Flags().StringSliceVar(&hosts, "hosts", nil, "Hosts a grant minted under the rule may name, comma-separated or repeated; defaults to the well-known credential's host, or the host the secret is bound to")
	cmd.Flags().Var(maxTTL, "max-grant-ttl", "Longest a grant minted under the rule may live: 1h, 3d, 2w, 1mo, or forever to leave it to the secret's limit (default 1h)")
	cmd.Flags().Var(expiresIn, "expires-in", "How long the rule itself approves for: 1h, 3d, 2w, 1mo (default: it does not lapse)")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "Create the rule switched off")
	return cmd
}

func (a *App) newSecretRuleUpdateCommand() *cobra.Command {
	maxTTL, expiresIn := lifetime.NewFlag(), lifetime.NewLapsingFlag()
	var statements, hosts, repositories []string
	definitions := policy.Definitions{}
	var disabled, neverExpires, everyDiscobox bool
	cmd := &cobra.Command{
		Use:   "update RULE_ID",
		Short: "Edit, enable or disable an approval rule",
		Long: `Edits an approval rule. Only what is given changes. A rule whose
repositories or policy changed is tested afresh against what is pending; the
grants it already minted are untouched, and disabling it (--disabled)
revokes nothing; --disabled=false switches it back on.

--statement replaces the rule's whole policy, so every statement it keeps is
given again, with a --define for each word that is not a default:
"discobox secret rule get" prints them in the words they are written in.

--repo replaces the repositories the rule is for. --every-discobox removes
them all, which makes it a rule for every discobox in the project, including
ones that do not exist yet; nothing else does.

A rule's credential and purpose are what it is, and are not edited: make
another rule.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			ruleID, err := a.resolveApprovalRuleID(cmd.Context(), client, projectID, args[0])
			if err != nil {
				return err
			}
			body := &apimodel.UpdateApprovalRuleBody{}
			flags := cmd.Flags()
			if flags.Changed("repo") || everyDiscobox {
				if body.Repositories, err = ruleRepositories(repositories, everyDiscobox, "--repo", "--every-discobox"); err != nil {
					return err
				}
				switch {
				case everyDiscobox:
					// Said in so many words: the server refuses an empty
					// list rather than read it as every discobox.
					body.SetEveryDiscobox(apiclientgen.NewOptBool(true))
				case len(body.Repositories) == 0:
					return errors.New("--repo names a repository; to make this a rule for every discobox say --every-discobox")
				}
			}
			written, err := writtenPolicy(statements, definitions, "--statement", "--define")
			switch {
			case err != nil:
				return err
			case written != nil:
				body.SetPolicy(apiclientgen.NewOptAccessPolicy(*written))
			}
			if flags.Changed("hosts") {
				if body.Hosts = grantHosts(hosts); len(body.Hosts) == 0 {
					return errors.New("a rule names at least one host: give --hosts a value")
				}
			}
			if seconds, given := maxTTL.Seconds(); given {
				body.SetMaxGrantTTLSeconds(apiclientgen.NewOptInt64(seconds))
			}
			expiresAfter, lapses := expiresIn.Duration()
			switch {
			case lapses && neverExpires:
				return errors.New("--expires-in and --never-expires cannot be used together")
			case lapses:
				body.SetExpiresAt(apiclientgen.NewOptDateTime(time.Now().Add(expiresAfter).UTC()))
			case neverExpires:
				body.SetNeverExpires(apiclientgen.NewOptBool(true))
			}
			if flags.Changed("disabled") {
				body.SetEnabled(apiclientgen.NewOptBool(!disabled))
			}
			res, err := client.UpdateApprovalRule(cmd.Context(), body, apiclientgen.UpdateApprovalRuleParams{ProjectId: projectID, RuleId: ruleID})
			if err != nil {
				return err
			}
			rule, err := expectResponse[apimodel.ApprovalRule](res)
			if err != nil {
				return err
			}
			return a.writeApprovalRule(cmd, rule)
		},
	}
	cmd.Flags().StringSliceVar(&repositories, "repo", nil, "Replace the repositories the rule is for with these, each a URL or host/path (github.com/org/repo), comma-separated or repeated")
	cmd.Flags().BoolVar(&everyDiscobox, "every-discobox", false, "Make this a rule for every discobox in the project, removing its repositories")
	cmd.Flags().StringArrayVar(&statements, "statement", nil, "Replace the rule's policy with these statements, each a sentence (repeatable)")
	cmd.Flags().Var(definitions, "define", "WORD=DEFINITION for a word the new statements use that is not a default (repeatable)")
	cmd.Flags().StringSliceVar(&hosts, "hosts", nil, "Replace the hosts a grant minted under the rule may name")
	cmd.Flags().Var(maxTTL, "max-grant-ttl", "Longest a grant minted under the rule may live: 1h, 3d, 2w, 1mo, or forever to leave it to the secret's limit")
	cmd.Flags().Var(expiresIn, "expires-in", "How long from now the rule approves for: 1h, 3d, 2w, 1mo")
	cmd.Flags().BoolVar(&neverExpires, "never-expires", false, "Clear the rule's expiry")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "Switch the rule off: it mints nothing more, and revokes nothing; --disabled=false switches it on")
	return cmd
}

func (a *App) newSecretRuleDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "rm RULE_ID...",
		Aliases: []string{"delete", "remove"},
		Short:   "Delete approval rules; the grants they minted stay",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			return runActionMany(cmd, args, "approval rule", "delete", "deleted", func(arg string) (string, error) {
				ruleID, err := a.resolveApprovalRuleID(cmd.Context(), client, projectID, arg)
				if err != nil {
					return "", err
				}
				res, err := client.DeleteApprovalRule(cmd.Context(), apiclientgen.DeleteApprovalRuleParams{ProjectId: projectID, RuleId: ruleID})
				if err != nil {
					return "", err
				}
				if err := expectNoContent[apiclientgen.DeleteApprovalRuleNoContent](res); err != nil {
					return "", err
				}
				return ruleID, nil
			})
		},
	}
}

// newSecretRequestProposeCommand shows what "auto approve" would offer for a
// request: the repository its rule would be for, and a policy the project's
// judge wrote from the request's, already held to covering it.
func (a *App) newSecretRequestProposeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "propose REQUEST_ID",
		Short: "Propose an approval rule for a request, already held to covering it",
		Long: `Proposes an approval rule for a pending agent credential request that asks
for a policy: the repository of the discobox that asked, which the rule would
be for, and a policy the project's judge wrote from the request's — the same
work on any target of its kind. The policy is then held to covering this
request, as the rule later would be: by code, with the judge reading only
conditions code cannot compare. One that does not cover it is shown as
failed, with why. The policy as asked needs no model, and is what the rule
approves when you give it no statements.

This can take a minute or two. Nothing is made: pass the statements you
choose to "discobox secret request approve --auto".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			requestID, err := a.resolveSecretRequestID(cmd.Context(), client, projectID, args[0])
			if err != nil {
				return err
			}
			res, err := client.ProposeApprovalRules(cmd.Context(), apiclientgen.ProposeApprovalRulesParams{ProjectId: projectID, RequestId: requestID})
			if err != nil {
				return err
			}
			proposals, err := expectResponse[apimodel.RuleProposalsBody](res)
			if err != nil {
				return err
			}
			return a.writeRuleProposals(cmd, requestID, proposals)
		},
	}
}

// autoApproval is the --auto flags of "secret request approve".
type autoApproval struct {
	auto          bool
	repositories  []string
	everyDiscobox bool
	statements    []string
	definitions   policy.Definitions
}

func (o *autoApproval) addFlags(cmd *cobra.Command) {
	o.definitions = policy.Definitions{}
	cmd.Flags().BoolVar(&o.auto, "auto", false, "Auto approve: make an approval rule from this request's policy and approve the request, its policy as asked, under it. Without --rule-repo or --rule-every-discobox the rule is for the repository of the discobox that asked; without --rule-statement it approves the policy as asked")
	cmd.Flags().StringSliceVar(&o.repositories, "rule-repo", nil, "With --auto: a repository the rule is for, as a URL or host/path (github.com/org/repo), comma-separated or repeated; the discobox that asked must be of one of them")
	cmd.Flags().BoolVar(&o.everyDiscobox, "rule-every-discobox", false, "With --auto: the rule is for every discobox in the project. It approves for discoboxes that do not exist yet, including ones a discobox creates")
	cmd.Flags().StringArrayVar(&o.statements, "rule-statement", nil, "With --auto: a statement of the policy the rule approves, as a sentence (repeatable); omit for the policy as asked")
	cmd.Flags().Var(o.definitions, "rule-define", "With --auto: WORD=DEFINITION for a word a --rule-statement uses that is not a default (repeatable)")
}

// rule is the rule the flags ask for, or nothing without --auto. The server
// copies the rule's credential, hosts and purpose from the request, names the
// asking discobox's repository when none is given here, takes the request's
// policy as asked when no statement is, and holds the policy to covering the
// request before it makes anything. A rule for every discobox is only ever
// asked for by its flag: the server refuses to assume one.
func (o *autoApproval) rule(cmd *cobra.Command) (apiclientgen.OptApproveWithRule, error) {
	flags := cmd.Flags()
	none := apiclientgen.OptApproveWithRule{}
	given := flags.Changed("rule-repo") || o.everyDiscobox || flags.Changed("rule-statement") || flags.Changed("rule-define")
	if !o.auto {
		if given {
			return none, errors.New("--rule-repo, --rule-every-discobox, --rule-statement and --rule-define say what an auto approval's rule is: add --auto")
		}
		return none, nil
	}
	repositories, err := ruleRepositories(o.repositories, o.everyDiscobox, "--rule-repo", "--rule-every-discobox")
	if err != nil {
		return none, err
	}
	written, err := writtenPolicy(o.statements, o.definitions, "--rule-statement", "--rule-define")
	if err != nil {
		return none, err
	}
	rule := apiclientgen.ApproveWithRule{Repositories: repositories}
	if written != nil {
		rule.SetPolicy(apiclientgen.NewOptAccessPolicy(*written))
	}
	switch {
	case flags.Changed("rule-repo") && len(repositories) == 0:
		return none, errors.New("--rule-repo names a repository; for a rule for every discobox say --rule-every-discobox")
	case o.everyDiscobox:
		rule.SetEveryDiscobox(apiclientgen.NewOptBool(true))
	}
	return apiclientgen.NewOptApproveWithRule(rule), nil
}

// ruleRepositories are a repository flag's values as a rule takes them:
// trimmed, with empty ones dropped. The server reads each and stores it as
// host/path. Naming repositories and asking for every discobox are two
// answers to one question, so both at once is refused.
func ruleRepositories(values []string, everyDiscobox bool, repoFlag, everyFlag string) ([]string, error) {
	var repositories []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			repositories = append(repositories, value)
		}
	}
	if everyDiscobox && len(repositories) > 0 {
		return nil, fmt.Errorf("%s and %s cannot be used together: a rule is for the repositories it names, or for every discobox", repoFlag, everyFlag)
	}
	return repositories, nil
}

func listApprovalRules(ctx context.Context, client *apiclientgen.Client, projectID string) ([]apimodel.ApprovalRule, error) {
	res, err := client.ListApprovalRules(ctx, apiclientgen.ListApprovalRulesParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListApprovalRulesBody](res)
	if err != nil {
		return nil, err
	}
	return body.GetApprovalRules(), nil
}

func (a *App) resolveApprovalRuleID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "approval rule ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	rules, err := listApprovalRules(ctx, client, projectID)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(rules))
	for _, rule := range rules {
		ids = append(ids, rule.ID)
	}
	return resolveShortID(id, "approval rule ID", ids)
}

// everyDiscobox is what a rule that names no repository is for, in the words
// the ADR has the card name it in.
const everyDiscobox = "every discobox in the project: approves for discoboxes that do not exist yet, including ones a discobox creates"

// ruleRepositoriesText says which discoboxes a rule is for.
func ruleRepositoriesText(rule *apimodel.ApprovalRule) string {
	if len(rule.Repositories) == 0 {
		return everyDiscobox
	}
	repositories := make([]string, 0, len(rule.Repositories))
	for _, repository := range rule.Repositories {
		repositories = append(repositories, terminalSafe(repository))
	}
	return "discoboxes of " + strings.Join(repositories, ", ")
}

// ruleRepositoriesColumn is ruleRepositoriesText for one cell of a listing.
func ruleRepositoriesColumn(rule *apimodel.ApprovalRule) string {
	if len(rule.Repositories) == 0 {
		return "every discobox"
	}
	return terminalSafe(strings.Join(rule.Repositories, ","))
}

func ruleCredentialText(rule *apimodel.ApprovalRule) string {
	if id := rule.WellKnownId.Or(""); id != "" {
		return id
	}
	return "secret " + rule.SecretId.Or("")
}

// rulePurposeText says what a rule hands out, which for a delegation is not
// what a reader would assume.
func rulePurposeText(purpose apiclientgen.ApprovalRulePurpose) string {
	if purpose == apiclientgen.ApprovalRulePurposeDelegate {
		return "delegate: approves asks to delegate the credential to other discoboxes, not to use it"
	}
	return string(purpose)
}

func ruleCapText(seconds int64) string {
	if seconds <= 0 {
		return "the secret's own limit"
	}
	return lifetime.Label(time.Duration(seconds) * time.Second)
}

func ruleExpiryText(rule *apimodel.ApprovalRule) string {
	if expiresAt, ok := rule.ExpiresAt.Get(); ok && !expiresAt.IsZero() {
		if !expiresAt.After(time.Now()) {
			return formatTime(expiresAt) + " (lapsed)"
		}
		return formatTime(expiresAt)
	}
	return "never"
}

func ruleStateText(rule *apimodel.ApprovalRule) string {
	if !rule.Enabled {
		return "disabled"
	}
	if expiresAt, ok := rule.ExpiresAt.Get(); ok && !expiresAt.IsZero() && !expiresAt.After(time.Now()) {
		return "lapsed"
	}
	return "enabled"
}

func (a *App) writeApprovalRule(cmd *cobra.Command, rule *apimodel.ApprovalRule) error {
	if rule == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), rule)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", rule.ID)
	fmt.Fprintf(tw, "STATE\t%s\n", ruleStateText(rule))
	fmt.Fprintf(tw, "CREDENTIAL\t%s\n", ruleCredentialText(rule))
	fmt.Fprintf(tw, "HOSTS\t%s\n", strings.Join(rule.Hosts, ", "))
	fmt.Fprintf(tw, "PURPOSE\t%s\n", rulePurposeText(rule.Purpose))
	fmt.Fprintf(tw, "FOR\t%s\n", ruleRepositoriesText(rule))
	// The rule's policy is what is approved; a person wrote or chose it, and
	// every statement is shown whole, in the words --statement writes it in.
	approves, err := apipolicy.From(rule.Policy)
	if err != nil {
		return err
	}
	policySentenceRows(tw, "POLICY", approves)
	if variables := approves.Variables(); len(variables) > 0 {
		fmt.Fprintf(tw, "VARIABLES\t%s, bound to whichever discobox asks\n", terminalSafe(strings.Join(variables, ", ")))
	}
	fmt.Fprintf(tw, "GRANTS LIVE AT MOST\t%s\n", ruleCapText(rule.MaxGrantTTLSeconds))
	fmt.Fprintf(tw, "EXPIRES\t%s\n", ruleExpiryText(rule))
	if by := rule.CreatedBy.Or(""); by != "" {
		fmt.Fprintf(tw, "MADE BY\t%s\n", by)
	}
	fmt.Fprintf(tw, "CREATED\t%s\n", formatTime(rule.CreatedAt))
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(rule.UpdatedAt))
	return tw.Flush()
}

func (a *App) writeApprovalRules(cmd *cobra.Command, rules []apimodel.ApprovalRule) error {
	rules = sortedByRecency(rules, func(rule apimodel.ApprovalRule) time.Time { return recencyTime(rule.UpdatedAt, rule.CreatedAt) })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), rules, func(rule apimodel.ApprovalRule) string { return rule.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"approvalRules": rules})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tCREDENTIAL\tHOSTS\tPURPOSE\tFOR\tPOLICY\tEXPIRES")
	for i := range rules {
		rule := &rules[i]
		approves, err := apipolicy.From(rule.Policy)
		if err != nil {
			return err
		}
		// The first statement, and how many more: "get" reads them all.
		var statements string
		if sentences := policy.FormatStatements(approves.Statements); len(sentences) > 0 {
			statements = terminalSafe(sentences[0])
			if more := len(sentences) - 1; more > 0 {
				statements = fmt.Sprintf("%s (+%d more)", statements, more)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			rule.ID,
			ruleStateText(rule),
			ruleCredentialText(rule),
			strings.Join(rule.Hosts, ","),
			rule.Purpose,
			ruleRepositoriesColumn(rule),
			statements,
			ruleExpiryText(rule),
		)
	}
	return tw.Flush()
}

// writeRuleProposals prints what auto approve offers. A proposal is a policy
// a model wrote from an agent's ask, so it is shown whole, escaped, with what
// holding it to the request found: its statements are what would be
// approved. Each is printed as the flag that sends it, quoted for the shell.
func (a *App) writeRuleProposals(cmd *cobra.Command, requestID string, proposals *apimodel.RuleProposalsBody) error {
	if proposals == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), proposals)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "A rule made from %s approves %s at %s, to %s.\n", requestID,
		terminalSafe(proposals.Credential.Or("the credential it asks for")), terminalSafe(strings.Join(proposals.Hosts, ", ")), proposals.Purpose)
	// What only a person otherwise approves, said before the policy: a rule
	// for a gate or a delegation hands it to every discobox it covers.
	if known, ok := wellknown.Lookup(proposals.Credential.Or("")); ok && known.Gate {
		fmt.Fprintf(out, "It hands on %s: a discobox it covers may create discoboxes, and hand them credentials, but only within what you separately delegate to it.\n", terminalSafe(known.Description))
	}
	if proposals.Purpose == apiclientgen.RuleProposalsBodyPurposeDelegate {
		fmt.Fprintln(out, "It hands on the power to delegate: a discobox it covers may give the credential to the discoboxes it creates, and may not use it itself.")
	}
	if note := proposals.Note.Or(""); note != "" {
		fmt.Fprintf(out, "\nNote: %s\n", terminalSafe(note))
	}
	fmt.Fprintln(out, "\nFOR — which discoboxes the rule is for")
	if repository := terminalSafe(proposals.Repository.Or("")); repository != "" {
		fmt.Fprintf(out, "  discoboxes of %s, the repository of the discobox that asked (the default with --auto)\n", repository)
		fmt.Fprintf(out, "     --rule-repo %s\n", repository)
	} else {
		fmt.Fprintln(out, "  the discobox that asked has no repository a rule could name: its source is a local one with no remote")
	}
	fmt.Fprintf(out, "  every discobox in the project, including ones that do not exist yet and ones a discobox creates\n     --rule-every-discobox\n")
	fmt.Fprintln(out, "\nPOLICY — what the rule approves")
	if written, ok := proposals.Policy.Get(); ok {
		fmt.Fprintln(out, "  written by the project's judge: the same work on any target of its kind")
		if proposed, ok := written.Policy.Get(); ok {
			read, err := apipolicy.From(proposed)
			if err != nil {
				return err
			}
			for _, sentence := range policy.FormatStatements(read.Statements) {
				fmt.Fprintf(out, "     --rule-statement %s\n", shellQuote(terminalSafe(sentence), false))
			}
			for _, word := range slices.Sorted(maps.Keys(read.Definitions)) {
				fmt.Fprintf(out, "     --rule-define %s\n", shellQuote(terminalSafe(word+"="+read.Definitions[word]), false))
			}
		} else {
			// What did not parse is no flag to pass: it is shown as written.
			for _, statement := range written.Statements {
				fmt.Fprintf(out, "     %s\n", terminalSafe(statement))
			}
		}
		said := "it covers this request"
		switch refused := terminalSafe(written.Refused.Or("")); {
		case refused != "":
			said = "FAILED: this could not be a rule's policy, so it was not held to the request — " + refused
		case !written.Covers:
			said = "FAILED: it does not cover this request; a rule with it as written would be refused"
		}
		if reason := terminalSafe(written.Reason.Or("")); reason != "" {
			said += " — " + reason
		}
		fmt.Fprintf(out, "     %s\n", said)
	} else {
		fmt.Fprintln(out, "  no model wrote one")
	}
	fmt.Fprintln(out, "  with no --rule-statement, the rule approves the policy as asked; an edited policy is held to covering the request when the rule is made")
	fmt.Fprintf(out, "\nTo make a rule: discobox secret request approve %s --auto [--rule-repo ... | --rule-every-discobox] [--rule-statement ... --rule-define ...]\n", requestID)
	return nil
}

// ruleTestText is one line of a request's card: a rule tested against it and
// what came of it.
func ruleTestText(test apimodel.RuleTest) string {
	stage := string(test.Stage.Or(""))
	switch {
	case test.Covered:
		// Covering is not the approval: the write can still be refused,
		// and the request then waits here.
		return fmt.Sprintf("%s: it covers this, and has not approved it yet", test.RuleId)
	case test.Pending.Or(false):
		return fmt.Sprintf("%s: its %s is still being judged", test.RuleId, stage)
	}
	reason := terminalSafe(test.Reason.Or(""))
	if reason == "" {
		reason = "no reason given"
	}
	return fmt.Sprintf("%s did not cover it (%s): %s", test.RuleId, stage, reason)
}
