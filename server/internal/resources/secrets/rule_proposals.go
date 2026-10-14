package secrets

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// "Auto approve" proposes one set of uses (ADR 26-10-12-330 §7): the
// request's own, written by the project's judge as the same work on any
// target of its kind — a typed proposal job, from the request's uses, which
// are data. A person reads it, and edits it, before it is a rule.
//
// Which discoboxes the rule is for is not proposed, because nothing about it
// is written: it is the repository of the discobox that asked, which the
// answer names, repositories the person names, or every discobox.
//
// What was written is put to the judge as the exact job a rule would later
// ask (§8), on the path that job takes, and its verdict is recorded and
// shown — so uses chosen as written approve the request under the verdict
// already on record, and edited ones are judged again on the approve. The
// credential, hosts and purpose are copied from the request by code, never
// generated.

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
		uses: useDescriptions(req.Uses), hosts: req.Hosts, purpose: req.Purpose, repository: repository,
	})
}

// ProposeApprovalRuleFromGrant is what a person reading a grant is shown to
// make a rule of it: the same proposal auto approve makes, from the request
// the grant answered — but written from the uses the grant approved, which
// approving may have changed, and for the grant's hosts and purpose. The
// written uses are judged against that request, as auto approve's are, so
// what the judge says is whether the rule would have approved this grant.
// The rule is made the way any is, and approves nothing already answered.
func (s *Service) ProposeApprovalRuleFromGrant(ctx context.Context, projectID, grantID string) (*services.RuleProposals, error) {
	if _, err := onlyAPerson(ctx); err != nil {
		return nil, err
	}
	grant, err := s.store.GetSecretGrant(ctx, projectID, grantID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret grant not found")
	}
	if len(grant.Uses) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"a rule approves uses, and this grant names none: a standing grant authorizes the credential without saying what for")
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
	return s.proposeRule(ctx, projectID, req, ruleSource{
		uses: useDescriptions(grant.Uses), hosts: grant.Hosts, purpose: grant.Purpose, repository: repository,
	})
}

// ruleSource is what a proposal is written from and judged against: the uses
// a rule must cover, and the facts the rule copies — from a pending request,
// or from a grant that answered one.
type ruleSource struct {
	uses       []string
	hosts      []string
	purpose    string
	repository string
}

// proposeRule writes a rule's uses from source's and puts them to the judge
// as the question a rule would ask of req. The credential is the request's,
// copied by code; the uses are a model's to write.
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
		// Nothing could read a rule either, so none could ever approve.
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "no judge can read an approval rule on this server")
	}
	// The uses are a model's to write. Where none can — Jev with jevUnsure:
	// refuse, no judge yet on a server that judges nothing else, a judge that
	// did not answer — there are none, and the note says why: the person
	// starts from the uses as asked, which the client already has.
	proposal, err := s.judge.ProposeRule(ctx, projectID, judge.ProposalJob{
		Uses:       source.uses,
		Credential: out.Credential.Or(""),
		Hosts:      source.hosts,
	})
	if err != nil {
		out.Note = apigen.NewOptString(fmt.Sprintf("no model wrote the uses: %s. Start from the uses as asked.", errorSentence(err)))
		return out, nil
	}
	written := apigen.RuleUsesProposal{Uses: proposal.UsesAnyTarget}
	// Held to what a rule may be before it is judged: nothing is shown as
	// covering that making the rule would then refuse. That is the server's
	// refusal, not the judge's reading, and is said as one.
	if problem := ruleUsesProblem(written.Uses); problem != "" {
		written.Refused = apigen.NewOptString(problem)
	} else {
		rule := &model.ApprovalRule{ProjectID: projectID, WellKnownID: req.WellKnownID, Hosts: source.hosts, Uses: written.Uses}
		ask, err := s.ruleAsk(ctx, rule, req)
		var answer judge.Answer
		if err == nil {
			ask.Uses = source.uses
			answer, err = s.judge.JudgeRule(ctx, projectID, ask)
		}
		written.Covers, written.Reason = err == nil && answer.Allow, apigen.NewOptString(verdictSentence(answer, err))
	}
	out.Uses = apigen.NewOptRuleUsesProposal(written)
	return out, nil
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
		// A request the proxy opened names no uses, and a rule's content is
		// uses read against uses.
		return apperrors.NewStatusError(http.StatusBadRequest,
			"an approval rule answers a request that names its uses; this one is a person's to answer each time")
	}
	return nil
}

// verdictSentence is what a candidate's card says the judge made of it.
func verdictSentence(answer judge.Answer, err error) string {
	if err != nil {
		return "the judge could not be asked: " + errorSentence(err)
	}
	if reason := strings.TrimSpace(answer.Reason); reason != "" {
		return reason
	}
	if answer.Allow {
		return "the judge found it covers this request"
	}
	return "the judge did not find it covers this request, and gave no reason"
}

// errorSentence is an error as one line a card can carry.
func errorSentence(err error) string {
	return strings.TrimSuffix(strings.TrimSpace(err.Error()), ".")
}
