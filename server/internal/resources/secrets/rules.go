package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/judges"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
)

// Approval rules (ADR 26-10-12-330): a project's standing answer to a
// credential request. A rule approves or says nothing — a request it does not
// cover waits for a person exactly as it does without one, and no rule denies.
//
// A request is held to a rule the way an approval is held to a delegation
// (delegated_approval.go): the repository the discobox is of, the credential,
// the hosts, the purpose and the lifetime are facts checked here, and whether
// what it asked for is within the rule's uses is a reading asked of the judge.

// The most a rule may say. A rule's sentences are read in every job about it.
const (
	maxRuleUses     = judge.MaxProposalUses
	maxRuleSentence = judge.MaxProposalSentence
)

// Dirtier marks a resource for reconciling. The reconcile engine is one; the
// service asks it to test rules against what is pending, and to look again at
// whether the project keeps a judge.
type Dirtier interface {
	MarkDirty(ctx context.Context, resourceType, id string) error
}

// SetDirtier gives the service the engine that runs its rule tests. Without
// one, no rule is tested.
func (s *Service) SetDirtier(dirtier Dirtier) { s.dirtier = dirtier }

// errRepositoriesAndEvery is a rule said to be for repositories and for every
// discobox at once, wherever one is made or edited.
var errRepositoriesAndEvery = apperrors.NewStatusError(http.StatusBadRequest,
	"a rule is for the repositories it names or for every discobox in the project, not both")

// onlyAPerson refuses a discobox: it never makes, edits, or reads a rule
// (ADR 26-10-12-330 §1). The sandbox role lists no rule route either; this is
// the same answer for a caller that reached the service another way.
func onlyAPerson(ctx context.Context) (auth.Principal, error) {
	principal, _ := auth.PrincipalFromContext(ctx)
	if principal.Type == auth.PrincipalTypeSandbox {
		return principal, apperrors.NewStatusError(http.StatusForbidden,
			"an approval rule is a person's: a discobox never makes, edits, or reads one")
	}
	return principal, nil
}

func (s *Service) ListApprovalRules(ctx context.Context, projectID string) ([]model.ApprovalRule, error) {
	if _, err := onlyAPerson(ctx); err != nil {
		return nil, err
	}
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	return s.store.ListApprovalRules(ctx, projectID)
}

func (s *Service) GetApprovalRule(ctx context.Context, projectID, ruleID string) (*model.ApprovalRule, error) {
	if _, err := onlyAPerson(ctx); err != nil {
		return nil, err
	}
	rule, err := s.store.GetApprovalRule(ctx, projectID, ruleID)
	if err != nil {
		return nil, apperrors.NotFound(err, "approval rule not found")
	}
	return rule, nil
}

// CreateApprovalRule makes a rule directly, as a pre-approval is made. It is
// the same thing answering a request with "auto approve" makes
// (ApproveSecretRequest), without a request to start from.
func (s *Service) CreateApprovalRule(ctx context.Context, projectID string, input services.CreateApprovalRuleBody) (*model.ApprovalRule, error) {
	principal, err := onlyAPerson(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	purpose, err := grantPurpose(string(input.Purpose.Or("")))
	if err != nil {
		return nil, err
	}
	// Which discoboxes the rule is for is said: the repositories it names, or
	// every discobox in so many words. Saying nothing is refused, never read
	// as the wider rule (§2).
	every := input.EveryDiscobox.Or(false)
	switch {
	case every && len(input.Repositories) > 0:
		return nil, errRepositoriesAndEvery
	case !every && len(input.Repositories) == 0:
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"a rule says which discoboxes it is for: the repositories it names, or everyDiscobox for every discobox in the project, which is never assumed")
	}
	rule := &model.ApprovalRule{
		ProjectID:    projectID,
		Repositories: input.Repositories,
		WellKnownID:  input.WellKnownId.Or(""),
		SecretID:     input.SecretId.Or(""),
		Hosts:        input.Hosts,
		Purpose:      purpose,
		Uses:         input.Uses,
		MaxGrantTTL:  input.MaxGrantTTLSeconds.Or(defaultMaxGrantTTLSeconds),
		CreatedBy:    grantedByOf(principal),
		Enabled:      input.Enabled.Or(true),
	}
	if expires, ok := input.ExpiresAt.Get(); ok {
		expires = expires.UTC()
		rule.ExpiresAt = &expires
	}
	if _, err := s.checkRule(ctx, rule, true); err != nil {
		return nil, err
	}
	if err := s.store.CreateApprovalRule(ctx, rule); err != nil {
		return nil, err
	}
	s.ruleChanged(ctx, projectID)
	return rule, nil
}

// UpdateApprovalRule edits a rule in place. Only what was sent changes. Its
// credential and purpose are what the rule is, and are not edited: a rule for
// another credential is another rule.
func (s *Service) UpdateApprovalRule(ctx context.Context, projectID, ruleID string, input services.UpdateApprovalRuleBody) (*model.ApprovalRule, error) {
	if _, err := onlyAPerson(ctx); err != nil {
		return nil, err
	}
	rule, err := s.store.GetApprovalRule(ctx, projectID, ruleID)
	if err != nil {
		return nil, apperrors.NotFound(err, "approval rule not found")
	}
	// Repositories sent replace the rule's, and every discobox is said in so
	// many words: a list sent empty is refused rather than read as the wider
	// rule, and one not sent leaves the rule's repositories as they are.
	switch every := input.EveryDiscobox.Or(false); {
	case every && input.Repositories != nil:
		return nil, errRepositoriesAndEvery
	case every:
		rule.Repositories = nil
	case input.Repositories != nil && len(input.Repositories) == 0:
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"an edit names at least one repository; to make this a rule for every discobox in the project say everyDiscobox, which naming nothing never means")
	case input.Repositories != nil:
		rule.Repositories = input.Repositories
	}
	if input.Uses != nil {
		rule.Uses = input.Uses
	}
	if input.Hosts != nil {
		rule.Hosts = input.Hosts
	}
	if seconds, ok := input.MaxGrantTTLSeconds.Get(); ok {
		rule.MaxGrantTTL = seconds
	}
	if enabled, ok := input.Enabled.Get(); ok {
		rule.Enabled = enabled
	}
	expires, expiring := input.ExpiresAt.Get()
	switch {
	case input.NeverExpires.Or(false) && expiring:
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a rule either lapses at expiresAt or never does, not both")
	case input.NeverExpires.Or(false):
		rule.ExpiresAt = nil
	case expiring:
		expires = expires.UTC()
		rule.ExpiresAt = &expires
	}
	// The expiry is held to the future only when this edit names one: a rule
	// that has already lapsed can still be disabled, or given more time.
	if _, err := s.checkRule(ctx, rule, expiring); err != nil {
		return nil, err
	}
	rule.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateApprovalRule(ctx, rule); err != nil {
		return nil, apperrors.NotFound(err, "approval rule not found")
	}
	// An edited rule is tested afresh against what is pending: its questions
	// are new ones, so nothing on record answers them (§4).
	s.ruleChanged(ctx, projectID)
	return rule, nil
}

// DeleteApprovalRule removes a rule. It mints nothing more and revokes
// nothing: the grants it made are independent of it, and list by rule to be
// revoked deliberately (§4).
func (s *Service) DeleteApprovalRule(ctx context.Context, projectID, ruleID string) error {
	if _, err := onlyAPerson(ctx); err != nil {
		return err
	}
	rule, err := s.store.GetApprovalRule(ctx, projectID, ruleID)
	if err != nil {
		return apperrors.NotFound(err, "approval rule not found")
	}
	if err := s.store.DeleteApprovalRule(ctx, projectID, rule.ID); err != nil {
		return apperrors.NotFound(err, "approval rule not found")
	}
	s.ruleChanged(ctx, projectID)
	return nil
}

// ruleChanged has a project's pending requests tested against its rules again,
// and its judge looked at again: a first rule is a reason to bring one up, and
// a last one gone may be the reason it had (§6). A mark that fails is only
// logged: the judge's own scan finds the project, and a request not tested
// waits for a person, as it would with no rule.
func (s *Service) ruleChanged(ctx context.Context, projectID string) {
	if s.dirtier == nil {
		return
	}
	// Past the caller's cancellation: the rule is written, and what follows
	// from it should not depend on the client still being there.
	ctx = context.WithoutCancel(ctx)
	if err := s.dirtier.MarkDirty(ctx, judges.JudgeResourceType, projectID); err != nil {
		logMarkFailure(ctx, "judge", projectID, err)
	}
	pending, err := s.store.ListRuleTestableRequests(ctx, projectID)
	if err != nil {
		logMarkFailure(ctx, RuleRequestResourceType, projectID, err)
		return
	}
	for _, requestID := range pending {
		if err := s.dirtier.MarkDirty(ctx, RuleRequestResourceType, requestID); err != nil {
			logMarkFailure(ctx, RuleRequestResourceType, requestID, err)
		}
	}
}

// requestFiled has a newly filed request tested against the project's rules,
// when it has any. A failure to mark is only logged: the request then waits
// for a person, as it would with no rule.
func (s *Service) requestFiled(ctx context.Context, req *model.SecretRequest) {
	if s.dirtier == nil || !ruleTestable(req) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	ruled, err := s.store.HasEnabledApprovalRule(ctx, req.ProjectID)
	if err != nil || !ruled {
		return
	}
	if err := s.dirtier.MarkDirty(ctx, RuleRequestResourceType, req.ID); err != nil {
		logMarkFailure(ctx, RuleRequestResourceType, req.ID, err)
	}
}

// checkRule holds a rule to what one may be, normalizing it in place, and
// returns the secret it names when it names one. It is the one check both ways
// of making a rule, and every edit, pass through. newExpiry says the expiry
// was named by this call, and so must still be ahead.
func (s *Service) checkRule(ctx context.Context, rule *model.ApprovalRule, newExpiry bool) (*model.Secret, error) {
	bad := func(format string, args ...any) (*model.Secret, error) {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(format, args...))
	}
	rule.WellKnownID = strings.TrimSpace(rule.WellKnownID)
	rule.SecretID = strings.TrimSpace(rule.SecretID)
	repositories, err := ruleRepositories(rule.Repositories)
	if err != nil {
		return bad("%s", err)
	}
	rule.Repositories = repositories

	// One credential, as a grant has one secret.
	var secret *model.Secret
	var defaultHosts []string
	switch {
	case (rule.WellKnownID == "") == (rule.SecretID == ""):
		return bad("a rule names one credential: a well-known ID, or a project secret")
	case rule.WellKnownID != "":
		known, ok := wellknown.Lookup(rule.WellKnownID)
		if !ok {
			return bad("%q is not a well-known credential", rule.WellKnownID)
		}
		defaultHosts = []string{known.Host()}
		for _, host := range rule.Hosts {
			if !known.AllowsHost(host) {
				return bad("%s is %s, which is sent to %s and the hosts beneath them, not %q",
					known.ID, known.Name, strings.Join(known.Hosts, ", "), host)
			}
		}
	default:
		found, err := s.store.GetSecret(ctx, rule.ProjectID, rule.SecretID)
		if err != nil {
			return nil, apperrors.NotFound(err, "secret not found")
		}
		secret, rule.SecretID = found, found.ID
		if found.Host != "" {
			defaultHosts = []string{found.Host}
		}
	}

	rule.Hosts = hostscope.List(rule.Hosts...)
	if len(rule.Hosts) == 0 {
		rule.Hosts = hostscope.List(defaultHosts...)
	}
	// A rule mints grants nobody reads first, so it names where they may go:
	// a wildcard grant stays an explicit act (`discobox secret grant create`).
	if len(rule.Hosts) == 0 {
		return bad("a rule names at least one host its grants may go to")
	}
	if secret != nil {
		if err := guardGrantHosts(secret, rule.Hosts); err != nil {
			return nil, err
		}
	}

	purpose, err := grantPurpose(rule.Purpose)
	if err != nil {
		return nil, err
	}
	rule.Purpose = purpose

	uses := make([]string, 0, len(rule.Uses))
	for _, use := range rule.Uses {
		if use = strings.TrimSpace(use); use != "" {
			uses = append(uses, use)
		}
	}
	rule.Uses = uses
	if problem := ruleUsesProblem(uses); problem != "" {
		return bad("%s", problem)
	}
	if rule.MaxGrantTTL < 0 {
		return bad("a rule's lifetime cap is a number of seconds; 0 leaves it to the secret's own limit")
	}
	if newExpiry && rule.ExpiresAt != nil && !rule.ExpiresAt.After(time.Now()) {
		return bad("a rule's expiry is in the future; leave it out for a rule that does not lapse")
	}
	return secret, nil
}

// ruleUsesProblem says why a list of uses could not be a rule's, or nothing
// when it could. A proposal is held to it before it is offered, so nothing is
// shown as covering that making the rule would refuse.
func ruleUsesProblem(uses []string) string {
	switch {
	case len(uses) == 0:
		return "a rule says what may be approved: at least one use"
	case len(uses) > maxRuleUses:
		return fmt.Sprintf("a rule names at most %d uses", maxRuleUses)
	}
	for _, use := range uses {
		// Uses reach the judge one per line.
		if len(use) > maxRuleSentence || strings.ContainsAny(use, "\r\n") {
			return fmt.Sprintf("each of a rule's uses is one sentence of at most %d bytes, on one line", maxRuleSentence)
		}
	}
	return ""
}

// plainSentence is a judge's reason as a refusal may carry it: one line, with
// nothing a terminal acts on. A reason is a model's words about an agent's,
// and a refusal is printed by whatever client asked.
func plainSentence(reason string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, reason))
}

// ruleCredential is the credential a rule job names to the judge: the
// well-known ID, which a person reads and an ask names, and nothing for a
// rule that names a secret — which secret answers is a fact already checked,
// and its name is not the asking discobox's to have read back.
func ruleCredential(rule *model.ApprovalRule) string { return rule.WellKnownID }

// ruleFacts holds a request to the facts of a rule (ADR 26-10-12-330 §§2–3):
// the same credential, a discobox of one of the rule's repositories, the
// purpose, and hosts within the rule's. repository is the asking discobox's
// (repositoryIdentity), empty when it has none. It returns the secret the rule
// would answer with, or the sentence saying which fact the request does not
// meet. applies is false for a rule that is not about this request at all —
// another credential's — which is not worth telling anybody.
//
// candidate is the secret a rule not yet made would name, as the approval
// about to make it will leave it; nil reads the rule's own.
//
// reading is the card being read rather than a rule being tested: nothing is
// made on the way, so a gate whose secret does not exist yet is left for the
// test to make. Nothing is made for a rule that is not for this discobox
// either.
func (s *Service) ruleFacts(ctx context.Context, rule *model.ApprovalRule, req *model.SecretRequest, repository string, candidate *model.Secret, reading bool) (secret *model.Secret, applies bool, reason string, err error) {
	elsewhere := ruleRepositoryProblem(rule, repository)
	secret = candidate
	if secret == nil {
		if secret, reason, err = s.ruleSecret(ctx, rule, reading || elsewhere != ""); err != nil {
			return nil, false, "", err
		}
		if secret == nil {
			// Worth saying only for a rule that names the credential asked
			// for: its secret is what is missing.
			return nil, rule.WellKnownID != "" && rule.WellKnownID == req.WellKnownID, reason, nil
		}
	}
	// The same credential: the well-known ID the request names. For an ask
	// that names none, hosts the rule's secret is bound for — a secret bound
	// nowhere says nothing about which free-form ask it answers.
	switch {
	case req.WellKnownID != "":
		if rule.WellKnownID != req.WellKnownID && secret.WellKnownID != req.WellKnownID {
			return nil, false, "the rule is for another credential", nil
		}
	case secret.Host == "" || !hostscope.CoversEvery([]string{secret.Host}, req.Hosts):
		// Not a rule about this ask unless its secret could be what answers
		// it; one bound elsewhere, or nowhere, is another credential's.
		return nil, false, "the rule's secret is not bound to a host covering the hosts asked for, and a rule recognizes an ask that names no well-known credential by its secret's binding", nil
	}
	// Which discoboxes the rule is for, compared and never read: a rule for
	// other repositories says so on the card, and no judge is asked.
	if elsewhere != "" {
		return nil, true, elsewhere, nil
	}
	// A refusal here is a fact the request does not meet, said as the reason,
	// not a failure of the test.
	requested, purposeErr := grantPurpose(req.Purpose)
	if purposeErr != nil {
		return nil, true, purposeErr.Error(), nil //nolint:nilerr // The refusal is the reason the rule does not cover the request, not a failed test.
	}
	if requested != rule.Purpose {
		return nil, true, fmt.Sprintf("the rule approves a credential to %s, and this asks to %s it", rule.Purpose, requested), nil
	}
	if !hostscope.CoversEvery(rule.Hosts, req.Hosts) {
		return nil, true, fmt.Sprintf("the rule's hosts (%s) do not cover every host asked for (%s)",
			strings.Join(rule.Hosts, ", "), strings.Join(req.Hosts, ", ")), nil
	}
	if outside := guardGrantHosts(secret, req.Hosts); outside != nil {
		return nil, true, outside.Error(), nil //nolint:nilerr // The refusal is the reason the rule does not cover the request, not a failed test.
	}
	return secret, true, "", nil
}

// ruleSecret is the secret a rule answers with: the one it names, or the
// project secret marked for its well-known ID — for a gate, made the first
// time it is needed, as approving a request for one makes it. A rule whose
// credential has no secret yet covers nothing, and reason says so.
func (s *Service) ruleSecret(ctx context.Context, rule *model.ApprovalRule, reading bool) (secret *model.Secret, reason string, err error) {
	if rule.SecretID != "" {
		secret, err = s.store.GetSecret(ctx, rule.ProjectID, rule.SecretID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, "the secret the rule names is gone", nil
		}
		return secret, "", err
	}
	known, ok := wellknown.Lookup(rule.WellKnownID)
	if !ok {
		return nil, fmt.Sprintf("%s is no longer a well-known credential", rule.WellKnownID), nil
	}
	if known.Gate && !reading {
		secret, err = s.gateSecret(ctx, rule.ProjectID, known)
		return secret, "", err
	}
	secret, err = s.store.FindSecretByWellKnownID(ctx, rule.ProjectID, rule.WellKnownID)
	if errors.Is(err, store.ErrNotFound) && known.Gate {
		return nil, fmt.Sprintf("%s has not been given in this project yet; the rule's first test makes it", rule.WellKnownID), nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Sprintf("no secret in the project answers %s yet; approve one request for it by hand, or store one with `discobox secret create --well-known %s`",
			rule.WellKnownID, rule.WellKnownID), nil
	}
	return secret, "", err
}

// ruleTTL is the lifetime of a grant a rule mints: fitted, never refused
// (§3) — what the request asked for, else the hour, capped by the rule and by
// the secret's limit.
func ruleTTL(rule *model.ApprovalRule, req *model.SecretRequest, secret *model.Secret) int64 {
	ttl := defaultApprovalTTL(req, secret)
	if rule.MaxGrantTTL > 0 && ttl > rule.MaxGrantTTL {
		ttl = rule.MaxGrantTTL
	}
	return ttl
}

// sameRuleText reports whether a rule still says what the judge read of it:
// the sentences, and the facts the request was held to.
func sameRuleText(read, now *model.ApprovalRule) bool {
	return slices.Equal(read.Repositories, now.Repositories) &&
		read.WellKnownID == now.WellKnownID && read.SecretID == now.SecretID &&
		read.Purpose == now.Purpose &&
		slices.Equal(read.Hosts, now.Hosts) && slices.Equal(read.Uses, now.Uses)
}

// ruleAsk is a rule's question about one request, composed the same way
// wherever it is asked — proposing, approving, reconciling, or reading the
// card — so one answer on record serves all four.
func ruleAsk(rule *model.ApprovalRule, req *model.SecretRequest) services.RuleAsk {
	return services.RuleAsk{
		RuleID:     rule.ID,
		RequestID:  req.ID,
		SandboxID:  req.SandboxID,
		RuleUses:   rule.Uses,
		Uses:       useDescriptions(req.Uses),
		Credential: ruleCredential(rule),
		Hosts:      rule.Hosts,
	}
}

// ruleUsesCover asks the judge a rule's one question about a request: whether
// every use asked for is within the rule's (§3). It is asked only of a rule
// the facts already hold the request to. An error is a judge that could not
// answer, which is not a no.
func (s *Service) ruleUsesCover(ctx context.Context, rule *model.ApprovalRule, req *model.SecretRequest) (covered bool, reason string, err error) {
	if s.judge == nil {
		return false, "", apperrors.NewStatusError(http.StatusServiceUnavailable, "no judge can read an approval rule on this server")
	}
	answer, err := s.judge.JudgeRule(ctx, rule.ProjectID, ruleAsk(rule, req))
	if err != nil {
		return false, "", err
	}
	return answer.Allow, answer.Reason, nil
}

// requesterRepository is the repository the discobox that filed a request is
// of, for its rules to be compared with. found is false when the discobox is
// gone, and a request nobody is waiting on is tested against nothing.
func (s *Service) requesterRepository(ctx context.Context, req *model.SecretRequest) (repository string, found bool, err error) {
	sandbox, err := s.store.GetSandbox(ctx, req.ProjectID, req.SandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return repositoryIdentity(sandbox.Source), true, nil
}

// liveRules are a project's rules that approve anything now, in the order
// they are tested: those that name repositories first, then those for every
// discobox, oldest first within each (§4), so the narrower rule is the one a
// grant names.
func (s *Service) liveRules(ctx context.Context, projectID string) ([]model.ApprovalRule, error) {
	rules, err := s.store.ListApprovalRules(ctx, projectID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	live := rules[:0]
	for _, rule := range rules {
		if rule.Live(now) {
			live = append(live, rule)
		}
	}
	slices.SortStableFunc(live, func(a, b model.ApprovalRule) int {
		switch {
		case len(a.Repositories) > 0 && len(b.Repositories) == 0:
			return -1
		case len(b.Repositories) > 0 && len(a.Repositories) == 0:
			return 1
		}
		return 0
	})
	return live, nil
}

// ruleTestable reports whether a rule may answer this request at all: an
// agent's own ask for a grant, still pending. A reactive request, a refresh
// request and a host trust stay a person's (§4), as does an ask that names no
// uses — there is nothing to read.
func ruleTestable(req *model.SecretRequest) bool {
	return req.Status == model.SecretRequestStatusPending && !req.IsRefresh() && req.FromProtocol() && req.SandboxID != ""
}

// ruleTestDescriber returns what fills in, for a person reading pending
// requests, each rule tested against one and why it did not cover it (§4). It
// asks nobody: it reads the rules and the verdicts already on record, and
// says a rule whose judge has still to answer is pending. A discobox is shown
// none of it.
//
// The project's rules are read once, the first time a request could be
// tested, and serve every request described after: the inbox is read on every
// beat of a window, and a project with no rules pays one query for it.
func (s *Service) ruleTestDescriber(ctx context.Context, projectID string) func(*model.SecretRequest) {
	if principal, _ := auth.PrincipalFromContext(ctx); principal.Type == auth.PrincipalTypeSandbox {
		return func(*model.SecretRequest) {}
	}
	var live []model.ApprovalRule
	loaded := false
	return func(req *model.SecretRequest) {
		if !ruleTestable(req) {
			return
		}
		if !loaded {
			live, _ = s.liveRules(ctx, projectID)
			loaded = true
		}
		if len(live) == 0 {
			return
		}
		repository, found, err := s.requesterRepository(ctx, req)
		if err != nil || !found {
			return
		}
		for i := range live {
			rule := &live[i]
			_, applies, reason, err := s.ruleFacts(ctx, rule, req, repository, nil, true)
			if err != nil || (!applies && reason != "") {
				continue
			}
			test := model.RuleTest{RuleID: rule.ID}
			if reason != "" {
				test.Stage, test.Reason = model.RuleTestStageFacts, reason
			} else {
				s.describeRuleReading(ctx, rule, req, &test)
			}
			req.RuleTests = append(req.RuleTests, test)
		}
	}
}

// describeRuleReading reads what the judge has already said about a rule's
// uses and a request's.
func (s *Service) describeRuleReading(ctx context.Context, rule *model.ApprovalRule, req *model.SecretRequest, test *model.RuleTest) {
	test.Stage = model.RuleTestStageContent
	digest, err := judge.Digest(ruleAsk(rule, req).Job())
	if err != nil {
		test.Pending = true
		return
	}
	// From a replica: the card is read on every beat of a window, and one
	// that has not caught up says "still being judged" for a beat longer.
	verdict, err := s.store.AnsweredQuestion(ctx, req.ProjectID, req.SandboxID, req.ID, digest, false)
	if err != nil {
		test.Pending = true
		return
	}
	test.Covered, test.Reason = verdict.Allow, verdict.Reason
}
