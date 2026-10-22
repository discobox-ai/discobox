package secrets

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/pkg/policy"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// "Auto approve" proposes one policy (ADR 26-10-12-330 §7): the request's
// own, written by the project's judge as the same work on any target of its
// kind — a typed proposal job, from the request's statements, which are data.
// A person reads it as sentences, and edits it, before it is a rule.
//
// Which discoboxes the rule is for is not proposed, because nothing about it
// is written: it is the repository of the discobox that asked, which the
// answer names, repositories the person names, or every discobox.
//
// What was written is parsed, validated, and held to covering the request
// exactly as a rule would later be (§8) — by code, with the judge asked about
// conditions on the path that question takes, its verdicts recorded — and
// what that found is shown: so a policy chosen as written approves the
// request under the verdicts already on record, and an edited one is held to
// it again on the approve. The credential, hosts and purpose are copied from
// the request by code, never generated; so are the definitions of the
// words, which the model cannot add to.

// ProposeApprovalRules is what a person choosing "auto approve" on a request
// is shown to choose from.
func (s *Service) ProposeApprovalRules(ctx context.Context, projectID, requestID string) (*services.RuleProposals, error) {
	if _, err := onlyAPerson(ctx); err != nil {
		return nil, err
	}
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret request not found")
	}
	if err := ruleProposable(req); err != nil {
		return nil, err
	}
	repository, found, err := s.requesterRepository(ctx, req)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "the discobox that filed the request was not found")
	}
	return s.proposeRule(ctx, projectID, req, ruleSource{
		policy: req.Policy, hosts: req.Hosts, purpose: req.Purpose, repository: repository,
	})
}

// ProposeApprovalRuleFromGrant is what a person reading a grant is shown to
// make a rule of it: the same proposal auto approve makes, from the request
// the grant answered — but written from the policy the grant approved, which
// approving may have changed, and for the grant's hosts and purpose. The
// written policy is held to covering the grant's, as auto approve's is held
// to the request's, so what it finds is whether the rule would have approved
// this grant.
// The rule is made the way any is, and approves nothing already answered.
func (s *Service) ProposeApprovalRuleFromGrant(ctx context.Context, projectID, grantID string) (*services.RuleProposals, error) {
	if _, err := onlyAPerson(ctx); err != nil {
		return nil, err
	}
	grant, err := s.store.GetSecretGrant(ctx, projectID, grantID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret grant not found")
	}
	switch {
	case grant.Policy == nil:
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"a rule approves a policy, and this grant carries none: it was approved for uses alone, or is a standing grant that authorizes the credential without saying what for")
	case grant.Purpose == model.SecretGrantPurposeDelegate:
		// As ruleProposable refuses a request to delegate, and checkRule the
		// rule: no rule approves a delegation yet (ADR 26-10-22-689 §4).
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"this grant delegates, and no rule approves a delegation yet; approve each delegation as a person")
	}
	req, err := s.store.GetSecretRequestByGrant(ctx, projectID, grant.ID)
	if err != nil {
		return nil, apperrors.NotFound(err, "the request this grant answered is not on record, and a rule is proposed from one: make the rule by hand")
	}
	// The discobox that asked may be gone, and nobody is waiting on this
	// request: there is then no repository to offer, and the person names one.
	repository, _, err := s.requesterRepository(ctx, req)
	if err != nil {
		return nil, err
	}
	// The grant's policy as approved, without the values it resolved to: it
	// is held to the rule as the request's would be, resolved afresh.
	approved := *grant.Policy
	approved.Resolved = nil
	return s.proposeRule(ctx, projectID, req, ruleSource{
		policy: &approved, hosts: grant.Hosts, purpose: grant.Purpose, repository: repository,
	})
}

// ruleSource is what a proposal is written from and held to: the policy a
// rule must cover, and the facts the rule copies — from a pending request,
// or from a grant that answered one.
type ruleSource struct {
	policy     *policy.Policy
	hosts      []string
	purpose    string
	repository string
}

// proposeRule writes a rule's policy from source's and holds it to covering
// that policy as a rule would hold req's. The credential is the request's,
// copied by code, and so are the definitions; the statements are a model's
// to write.
func (s *Service) proposeRule(ctx context.Context, projectID string, req *model.SecretRequest, source ruleSource) (*services.RuleProposals, error) {
	purpose, err := grantPurpose(source.purpose)
	if err != nil {
		return nil, err
	}
	out := &services.RuleProposals{
		Hosts:   source.hosts,
		Purpose: apigen.RuleProposalsBodyPurpose(purpose),
	}
	if source.repository != "" {
		out.Repository = apigen.NewOptString(source.repository)
	}
	if credential := ruleCredential(&model.ApprovalRule{WellKnownID: req.WellKnownID}); credential != "" {
		out.Credential = apigen.NewOptString(credential)
		// Said apart from the name: a rule for the ID is the rule judged
		// below, and one for the secret that happened to answer it is not.
		out.WellKnownId = apigen.NewOptString(req.WellKnownID)
	} else if req.Name != "" {
		out.Credential = apigen.NewOptString(req.Name)
	}

	if s.judge == nil {
		// No model writes here. A rule still approves what code alone finds
		// within it, so the person starts from the policy as asked.
		out.Note = apigen.NewOptString("no model can write a policy on this server. Start from the policy as asked.")
		return out, nil
	}
	// The statements are a model's to write. Where none can — Jev with
	// jevUnsure: refuse, no judge yet on a server that judges nothing else, a
	// judge that did not answer — there are none, and the note says why: the
	// person starts from the policy as asked, which the client already has.
	proposal, err := s.judge.ProposeRule(ctx, projectID, judge.ProposalJob{
		Statements:  policy.FormatStatements(source.policy.Statements),
		Definitions: source.policy.Definitions,
		Credential:  out.Credential.Or(""),
		Hosts:       source.hosts,
	})
	if err != nil {
		out.Note = apigen.NewOptString(fmt.Sprintf("no model wrote a policy: %s. Start from the policy as asked.", errorSentence(err)))
		return out, nil
	}
	written, candidate := writtenPolicy(proposal.StatementsAnyTarget, source.policy.Definitions)
	// Held to what a rule may be before it is compared: nothing is shown as
	// covering that making the rule would then refuse. That is the server's
	// refusal, not the judge's reading, and is said as one.
	if candidate != nil {
		rule := &model.ApprovalRule{ProjectID: projectID, WellKnownID: req.WellKnownID, Hosts: source.hosts, Policy: candidate}
		coverage, err := s.rulePolicyCovers(ctx, rule, req, source.policy, false)
		switch {
		case err != nil:
			written.Reason = apigen.NewOptString("the policy could not be compared with the one asked for: " + errorSentence(err))
		case coverage.covered:
			// Covering the request is not enough: a rule that dropped a deny
			// asked for would approve later asks without it. The model is
			// told to keep every one, and is held to it here.
			if err := policy.CarriesDenies(*candidate, candidate.Resolve(coverage.facts).Resolved, *source.policy, coverage.values); err != nil {
				written.Reason = apigen.NewOptString("the policy written drops a deny the request asked for: " + err.Error())
				break
			}
			written.Covers, written.Reason = true, apigen.NewOptString(coverage.reason)
		default:
			written.Reason = apigen.NewOptString(coverage.reason)
		}
	}
	out.Policy = apigen.NewOptRulePolicyProposal(written)
	return out, nil
}

// writtenPolicy is the policy a model wrote, as a proposal shows it, and the
// policy itself when it is one a rule may have (judge.WrittenPolicy): its
// definitions are the source's, so a word the model brought in has none and
// is refused, as is a statement that does not parse.
func writtenPolicy(sentences []string, definitions map[string]string) (apigen.RulePolicyProposal, *policy.Policy) {
	refuse := func(why string) (apigen.RulePolicyProposal, *policy.Policy) {
		return apigen.RulePolicyProposal{Statements: sentences, Refused: apigen.NewOptString(why)}, nil
	}
	written, err := judge.WrittenPolicy(sentences, definitions)
	if err != nil {
		return refuse("policy: " + strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	shown, err := services.APIPolicy(&written)
	if err != nil {
		return refuse(err.Error())
	}
	return apigen.RulePolicyProposal{Policy: shown}, &written
}

// ruleProposable says why a rule cannot be made from this request, or nothing
// when one can.
func ruleProposable(req *model.SecretRequest) error {
	switch {
	case req.Status != model.SecretRequestStatusPending:
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("secret request is already %s", req.Status))
	case req.IsRefresh():
		return apperrors.NewStatusError(http.StatusBadRequest, "a refresh request asks for a new value; there is no approval to make a rule of")
	case !req.FromProtocol() || req.SandboxID == "":
		// A request the proxy opened asks for no policy, and a rule's content
		// is a policy held to a policy.
		return apperrors.NewStatusError(http.StatusBadRequest,
			"an approval rule answers a request that asks for a policy; this one is a person's to answer each time")
	case req.Purpose == model.SecretGrantPurposeDelegate:
		// As checkRule refuses: no rule delegates yet (ADR 26-10-22-689,
		// Deferred).
		return apperrors.NewStatusError(http.StatusBadRequest,
			"this request asks to delegate, and no rule approves a delegation yet; approve it as a person, without a rule")
	case req.Policy == nil:
		// A rule's policy is what a request's is held to (ruleTestable): a
		// rule made from uses alone would have nothing to hold later asks to
		// (ADR 26-10-22-689).
		return apperrors.NewStatusError(http.StatusBadRequest,
			"this request asks for uses alone, and an approval rule answers a request's policy; approve it as a person, without a rule")
	}
	return nil
}

// errorSentence is an error as one line a card can carry.
func errorSentence(err error) string {
	return strings.TrimSuffix(strings.TrimSpace(err.Error()), ".")
}
