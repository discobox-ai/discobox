package secrets

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// "Auto approve" proposes a rule's uses at three levels, narrowest first
// (ADR 26-10-12-330 §7). The narrowest needs no model: the request's uses as
// asked. The wider two are written by the project's judge as a typed proposal
// job, from the request's uses, which are data.
//
// Which discoboxes the rule is for is not proposed, because nothing about it
// is written: it is the repository of the discobox that asked, which the
// answer names, or every discobox in the project.
//
// A proposal is offered only once the judge has found it covers the request
// (§8). Each candidate is put to the judge as the exact job a rule would
// later ask, on the path that job takes, and its verdict is recorded — so
// choosing a candidate approves the request under the verdict already on
// record. The credential, hosts and purpose are copied from the request by
// code, never generated.

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
	purpose, err := grantPurpose(req.Purpose)
	if err != nil {
		return nil, err
	}
	asked := useDescriptions(req.Uses)

	out := &services.RuleProposals{
		Hosts:    req.Hosts,
		Purpose:  apigen.RuleProposalsBodyPurpose(purpose),
		Contents: []apigen.RuleContentProposal{{Level: 1, Uses: asked}},
	}
	if repository != "" {
		out.Repository = apigen.NewOptString(repository)
	}
	if credential := ruleCredential(&model.ApprovalRule{WellKnownID: req.WellKnownID}); credential != "" {
		out.Credential = apigen.NewOptString(credential)
	} else if req.Name != "" {
		out.Credential = apigen.NewOptString(req.Name)
	}

	if s.judge == nil {
		// Nothing could read a rule either, so none could ever approve.
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "no judge can read an approval rule on this server")
	}
	// The wider levels are a model's to write. Where none can — Jev with
	// jevUnsure: refuse, no judge yet on a server that judges nothing else, a
	// judge that did not answer — the uses as asked are offered alone, and
	// wider ones can be written by hand.
	proposal, err := s.judge.ProposeRule(ctx, projectID, judge.ProposalJob{
		Uses:       asked,
		Credential: out.Credential.Or(""),
		Hosts:      req.Hosts,
	})
	if err != nil {
		out.Note = apigen.NewOptString(fmt.Sprintf("only the uses as asked are offered, because no model wrote wider ones: %s. Wider uses can be written by hand.", errorSentence(err)))
	} else {
		out.Contents = append(out.Contents,
			apigen.RuleContentProposal{Level: 2, Uses: proposal.UsesAnyTarget},
			apigen.RuleContentProposal{Level: 3, Uses: proposal.UsesAcrossResource})
	}

	// Every candidate is tested, the narrowest included: a judge that will
	// not find sentences within themselves is a rule that will never approve
	// a repeat ask, and that is worth knowing before it is made (§8). They
	// are independent questions, so they are asked together.
	var asks sync.WaitGroup
	for i := range out.Contents {
		candidate := &out.Contents[i]
		// Held to what a rule may be before it is judged: nothing is offered
		// as covering that making the rule would then refuse.
		if problem := ruleUsesProblem(candidate.Uses); problem != "" {
			candidate.Reason = apigen.NewOptString(problem)
			continue
		}
		asks.Add(1)
		go func() {
			defer asks.Done()
			rule := &model.ApprovalRule{ProjectID: projectID, WellKnownID: req.WellKnownID, Hosts: req.Hosts, Uses: candidate.Uses}
			answer, err := s.judge.JudgeRule(ctx, projectID, ruleAsk(rule, req))
			candidate.Covers, candidate.Reason = err == nil && answer.Allow, apigen.NewOptString(verdictSentence(answer, err))
		}()
	}
	asks.Wait()
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
