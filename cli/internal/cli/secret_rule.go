package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/wellknown"
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
in the project when it names none. Its uses are sentences the judge reads
against the uses the request asks for.

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
	var wellKnown, secretRef, maxTTL, expiresIn string
	var uses, hosts, repositories []string
	var delegate, disabled, everyDiscobox bool
	cmd := &cobra.Command{
		Use:   "create (--well-known ID | --secret SECRET) (--repo REPOSITORY... | --every-discobox) --use SENTENCE...",
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
including ones a discobox creates, and is never the default.`,
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
			body := &apimodel.CreateApprovalRuleBody{Uses: ruleSentences(uses), Hosts: grantHosts(hosts)}
			if len(body.Uses) == 0 {
				return errors.New("a rule says what may be approved: give at least one --use")
			}
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
			if delegate {
				body.SetPurpose(apiclientgen.NewOptCreateApprovalRuleBodyPurpose(apiclientgen.CreateApprovalRuleBodyPurposeDelegate))
			}
			seconds, given, err := grantLifetime(cmd.Flags(), "max-grant-ttl", maxTTL)
			if err != nil {
				return err
			}
			if given {
				body.SetMaxGrantTTLSeconds(apiclientgen.NewOptInt64(seconds))
			}
			expires, lapses, err := ruleExpiry(cmd, expiresIn)
			if err != nil {
				return err
			}
			if lapses {
				body.SetExpiresAt(apiclientgen.NewOptDateTime(expires))
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
	cmd.Flags().StringArrayVar(&uses, "use", nil, "What may be approved, as a sentence the judge reads the uses a request asks for against (repeatable)")
	cmd.Flags().StringSliceVar(&hosts, "hosts", nil, "Hosts a grant minted under the rule may name, comma-separated or repeated; defaults to the well-known credential's host, or the host the secret is bound to")
	cmd.Flags().BoolVar(&delegate, "delegate", false, "Approve asks to delegate the credential to other discoboxes, rather than asks to use it")
	cmd.Flags().StringVar(&maxTTL, "max-grant-ttl", "", "Longest a grant minted under the rule may live: 1h, 3d, 2w, 1mo, or forever to leave it to the secret's limit (default 1h)")
	cmd.Flags().StringVar(&expiresIn, "expires-in", "", "How long the rule itself approves for: 1h, 3d, 2w, 1mo (default: it does not lapse)")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "Create the rule switched off")
	return cmd
}

func (a *App) newSecretRuleUpdateCommand() *cobra.Command {
	var maxTTL, expiresIn string
	var uses, hosts, repositories []string
	var enable, disable, neverExpires, everyDiscobox bool
	cmd := &cobra.Command{
		Use:   "update RULE_ID",
		Short: "Edit, enable or disable an approval rule",
		Long: `Edits an approval rule. Only what is given changes. A rule whose
repositories or uses changed is tested afresh against what is pending; the
grants it already minted are untouched, and disabling it revokes nothing.

--repo replaces the repositories the rule is for. --every-discobox removes
them all, which makes it a rule for every discobox in the project, including
ones that do not exist yet; nothing else does.

A rule's credential and purpose are what it is, and are not edited: make
another rule.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if enable && disable {
				return errors.New("--enable and --disable cannot be used together")
			}
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
			if flags.Changed("use") {
				if body.Uses = ruleSentences(uses); len(body.Uses) == 0 {
					return errors.New("a rule says what may be approved: give at least one --use")
				}
			}
			if flags.Changed("hosts") {
				if body.Hosts = grantHosts(hosts); len(body.Hosts) == 0 {
					return errors.New("a rule names at least one host: give --hosts a value")
				}
			}
			seconds, given, err := grantLifetime(flags, "max-grant-ttl", maxTTL)
			if err != nil {
				return err
			}
			if given {
				body.SetMaxGrantTTLSeconds(apiclientgen.NewOptInt64(seconds))
			}
			expires, lapses, err := ruleExpiry(cmd, expiresIn)
			switch {
			case err != nil:
				return err
			case lapses && neverExpires:
				return errors.New("--expires-in and --never-expires cannot be used together")
			case lapses:
				body.SetExpiresAt(apiclientgen.NewOptDateTime(expires))
			case neverExpires:
				body.SetNeverExpires(apiclientgen.NewOptBool(true))
			}
			if enable || disable {
				body.SetEnabled(apiclientgen.NewOptBool(enable))
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
	cmd.Flags().StringArrayVar(&uses, "use", nil, "Replace what may be approved with these sentences (repeatable)")
	cmd.Flags().StringSliceVar(&hosts, "hosts", nil, "Replace the hosts a grant minted under the rule may name")
	cmd.Flags().StringVar(&maxTTL, "max-grant-ttl", "", "Longest a grant minted under the rule may live: 1h, 3d, 2w, 1mo, or forever to leave it to the secret's limit")
	cmd.Flags().StringVar(&expiresIn, "expires-in", "", "How long from now the rule approves for: 1h, 3d, 2w, 1mo")
	cmd.Flags().BoolVar(&neverExpires, "never-expires", false, "Clear the rule's expiry")
	cmd.Flags().BoolVar(&enable, "enable", false, "Switch the rule on")
	cmd.Flags().BoolVar(&disable, "disable", false, "Switch the rule off: it mints nothing more, and revokes nothing")
	return cmd
}

func (a *App) newSecretRuleDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete RULE_ID...",
		Short: "Delete approval rules; the grants they minted stay",
		Args:  cobra.MinimumNArgs(1),
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
// request: the repository its rule would be for, and three lists of uses, each
// already put to the judge.
func (a *App) newSecretRequestProposeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "propose REQUEST_ID",
		Short: "Propose approval rules for a request, each already judged against it",
		Long: `Proposes approval rules for a pending agent credential request: the
repository of the discobox that asked, which the rule would be for, and three
lists of uses, narrowest first. The uses as asked need no model. The wider
ones are written by the project's judge, and each is then put to the judge as
the question a rule would later ask, so a proposal that does not cover this
request is shown as failed, with why.

This can take a minute or two. Nothing is made: pass the sentences you choose
to "discobox secret request approve --auto".`,
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
	uses          []string
}

func (o *autoApproval) addFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&o.auto, "auto", false, "Auto approve: make an approval rule from this request and approve the request under it. Without --rule-repo or --rule-every-discobox the rule is for the repository of the discobox that asked; without --rule-use it approves the uses as asked")
	cmd.Flags().StringSliceVar(&o.repositories, "rule-repo", nil, "With --auto: a repository the rule is for, as a URL or host/path (github.com/org/repo), comma-separated or repeated; the discobox that asked must be of one of them")
	cmd.Flags().BoolVar(&o.everyDiscobox, "rule-every-discobox", false, "With --auto: the rule is for every discobox in the project. It approves for discoboxes that do not exist yet, including ones a discobox creates")
	cmd.Flags().StringArrayVar(&o.uses, "rule-use", nil, "With --auto: what the rule approves, as a sentence the judge reads (repeatable); omit for the uses as asked")
}

// rule is the rule the flags ask for, or nothing without --auto. The server
// copies the rule's credential, hosts and purpose from the request, names the
// asking discobox's repository when none is given here, and holds the uses to
// the judge before it makes anything. A rule for every discobox is only ever
// asked for by its flag: the server refuses to assume one.
func (o *autoApproval) rule(cmd *cobra.Command) (apiclientgen.OptApproveWithRule, error) {
	flags := cmd.Flags()
	none := apiclientgen.OptApproveWithRule{}
	given := flags.Changed("rule-repo") || o.everyDiscobox || flags.Changed("rule-use")
	if !o.auto {
		if given {
			return none, errors.New("--rule-repo, --rule-every-discobox and --rule-use say what an auto approval's rule is: add --auto")
		}
		return none, nil
	}
	repositories, err := ruleRepositories(o.repositories, o.everyDiscobox, "--rule-repo", "--rule-every-discobox")
	if err != nil {
		return none, err
	}
	rule := apiclientgen.ApproveWithRule{Uses: ruleSentences(o.uses), Repositories: repositories}
	switch {
	case flags.Changed("rule-repo") && len(repositories) == 0:
		return none, errors.New("--rule-repo names a repository; for a rule for every discobox say --rule-every-discobox")
	case flags.Changed("rule-use") && len(rule.Uses) == 0:
		return none, errors.New("--rule-use is a sentence; omit it to approve the uses as asked")
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
	repositories := ruleSentences(values)
	if everyDiscobox && len(repositories) > 0 {
		return nil, fmt.Errorf("%s and %s cannot be used together: a rule is for the repositories it names, or for every discobox", repoFlag, everyFlag)
	}
	return repositories, nil
}

// ruleSentences are --use values as a rule takes them: trimmed, with empty
// ones dropped.
func ruleSentences(values []string) []string {
	var out []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// ruleExpiry reads --expires-in as the instant a rule stops approving. A rule
// that never lapses is said by leaving the flag out, not by "forever": zero
// would otherwise be an expiry already past.
func ruleExpiry(cmd *cobra.Command, value string) (time.Time, bool, error) {
	if !cmd.Flags().Changed("expires-in") {
		return time.Time{}, false, nil
	}
	d, err := lifetime.Parse(value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("--expires-in: %w", err)
	}
	if d <= 0 {
		return time.Time{}, false, errors.New("--expires-in is how long the rule approves for; leave it out (or --never-expires) for a rule that does not lapse")
	}
	return time.Now().Add(d).UTC(), true, nil
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
	// The rule's sentences are what is approved; a person wrote or chose
	// them, and they are shown whole.
	for i, use := range rule.Uses {
		label := "USES"
		if i > 0 {
			label = ""
		}
		fmt.Fprintf(tw, "%s\t%s\n", label, terminalSafe(use))
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
	fmt.Fprintln(tw, "ID\tSTATE\tCREDENTIAL\tHOSTS\tPURPOSE\tFOR\tUSES\tEXPIRES")
	for i := range rules {
		rule := &rules[i]
		uses := terminalSafe(rule.Uses[0])
		if more := len(rule.Uses) - 1; more > 0 {
			uses = fmt.Sprintf("%s (+%d more)", uses, more)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			rule.ID,
			ruleStateText(rule),
			ruleCredentialText(rule),
			strings.Join(rule.Hosts, ","),
			rule.Purpose,
			ruleRepositoriesColumn(rule),
			uses,
			ruleExpiryText(rule),
		)
	}
	return tw.Flush()
}

// writeRuleProposals prints what auto approve offers. A proposal is a
// sentence a model wrote from an agent's ask, so every one is shown whole,
// escaped, with what the judge made of it: the text is what would be approved.
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
	// What only a person otherwise approves, said before the sentences: a
	// rule for a gate or a delegation hands it to every discobox it covers.
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
	fmt.Fprintln(out, "\nUSES — what the rule approves")
	if written, ok := proposals.Uses.Get(); ok {
		fmt.Fprintln(out, "  written by the project's judge: the same work on any target of its kind")
		for _, use := range written.Uses {
			fmt.Fprintf(out, "     --rule-use %q\n", terminalSafe(use))
		}
		said := "the judge found these cover this request"
		switch refused := terminalSafe(written.Refused.Or("")); {
		case refused != "":
			said = "FAILED: these could not be a rule's, so the judge was not asked — " + refused
		case !written.Covers:
			said = "FAILED: the judge found these do not cover this request; a rule with them as written would be refused"
		}
		if reason := terminalSafe(written.Reason.Or("")); reason != "" {
			said += " — " + reason
		}
		fmt.Fprintf(out, "     %s\n", said)
	} else {
		fmt.Fprintln(out, "  no model wrote any")
	}
	fmt.Fprintln(out, "  with no --rule-use, the rule approves the uses as asked; edited uses are judged when the rule is made")
	fmt.Fprintf(out, "\nTo make a rule: discobox secret request approve %s --auto [--rule-repo ... | --rule-every-discobox] [--rule-use ...]\n", requestID)
	return nil
}

// ruleTestText is one line of a request's card: a rule tested against it and
// what came of it.
func ruleTestText(test apimodel.RuleTest) string {
	stage := string(test.Stage.Or(""))
	switch {
	case test.Covered:
		// The judge's yes is not the approval: the write can still be
		// refused, and the request then waits here.
		return fmt.Sprintf("%s: the judge found it covers this, and it has not approved it yet", test.RuleId)
	case test.Pending.Or(false):
		return fmt.Sprintf("%s: its %s is still being judged", test.RuleId, stage)
	}
	reason := terminalSafe(test.Reason.Or(""))
	if reason == "" {
		reason = "no reason given"
	}
	return fmt.Sprintf("%s did not cover it (%s): %s", test.RuleId, stage, reason)
}
