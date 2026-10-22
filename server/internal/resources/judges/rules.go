package judges

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandboxagentclient"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// An approval rule's reading, and the policy a person starts one from
// (ADR 26-10-12-330). Which discoboxes a rule is for is a list of repositories
// the secrets service compares, and so is most of its policy (ADR
// 26-10-22-689): only the conditions
// of an allow asked for, against an allow of the rule that covers it but for
// them, are read here.
//
// Rule jobs hang on neither judgeCommands nor judgeCredentials (§6): a rule is
// its own opt-in, and a job is refused only when there is nothing to ask. A
// project keeps a judge discobox while it has a rule switched on (wanted).
//
// Each question is answered once. The verdict records the digest of the job
// that was judged, and the same job asked again — the candidate a person chose
// from a proposal, a reconcile run after one that already asked — is answered
// from the record. An edited rule is a different job.

// JudgeRule asks whether the conditions of an allow a request asked for are
// within those of a rule's allow:
// from the record when the same job was already judged, and otherwise by
// asking and recording. It goes the way every job does: Jev first when the
// server has a key, what Jev is unsure of handled by jevUnsure, and the judge
// discobox otherwise (§5).
//
// Every way of not answering is no verdict, and nothing is recorded for it,
// so the question is asked again later: an answer on record is never asked
// about again, and a judge that did not decide must not become one.
func (s *Service) JudgeRule(ctx context.Context, projectID string, ask services.RuleAsk) (judge.Answer, error) {
	row := &model.CredentialVerdict{
		Kind:            model.CredentialVerdictKindRule,
		SandboxID:       ask.SandboxID,
		RuleID:          ask.RuleID,
		SecretRequestID: ask.RequestID,
	}
	job := ask.Job
	if err := job.Validate(); err != nil {
		return judge.Answer{}, apperrors.NewStatusError(http.StatusBadRequest, "a rule cannot be judged: "+err.Error())
	}
	prompt, err := judge.Prompt(job)
	if err != nil {
		return judge.Answer{}, err
	}
	digest, err := judge.Digest(job)
	if err != nil {
		return judge.Answer{}, err
	}
	recorded, err := s.store.AnsweredQuestion(ctx, projectID, row.SandboxID, row.SecretRequestID, digest, true)
	switch {
	case err == nil:
		return s.reuse(ctx, recorded, row)
	case !errors.Is(err, store.ErrNotFound):
		return judge.Answer{}, err
	}
	if s.jev == nil && s.leases == nil {
		return judge.Answer{}, apperrors.NewStatusError(http.StatusServiceUnavailable, "this server cannot reach a judge")
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return judge.Answer{}, apperrors.NotFound(err, "project not found")
	}
	bound := min(s.judgeBound(), services.DelegationBound)
	judgeCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	var judgeSandbox *model.Sandbox
	if s.jev == nil {
		if judgeSandbox, err = s.readyJudge(judgeCtx, project); err != nil {
			return judge.Answer{}, err
		}
	}
	decided, err := s.put(judgeCtx, project, judgeSandbox, job, bound)
	if err != nil {
		return judge.Answer{}, err
	}
	// An answer on record is reused for good, so only a decision is recorded.
	// The job has no body to be shown, so an ask for one decides nothing.
	// And where what Jev is unsure of goes on to the judge discobox
	// (ADR 26-10-12-669), an unsure refusal that comes back as Jev's alone is
	// one the discobox did not answer — not up yet, or out of time — so the
	// one who decides has not: it is no verdict, and the question is asked
	// again. A refusal Jev was sure of is final, and is recorded.
	switch {
	case !decided.Decided():
		return judge.Answer{}, apperrors.NewStatusError(http.StatusBadGateway,
			"the judge asked to be shown something a rule's question does not have, which decides nothing")
	case s.jevFallback && s.leases != nil && decided.unsure && decided.sandbox == nil:
		return judge.Answer{}, apperrors.NewStatusError(http.StatusServiceUnavailable,
			"Jev could not tell and the project's judge discobox, which decides what Jev is unsure of, did not answer")
	}
	// Nothing stands for a rule job: each is its own question.
	decided.Standing = nil

	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancelRecord()
	row.ProjectID = project.ID
	row.Origin = model.CredentialVerdictOriginJudge
	row.Round = job.Round
	row.Allow = decided.Allow
	row.Reason = decided.Reason
	row.Prompt = prompt
	row.QuestionDigest = digest
	decided.stamp(row)
	// Recorded before the answer is acted on: a grant minted on an answer
	// with no record of it is one nobody can trace (§5).
	if err := s.store.CreateCredentialVerdict(recordCtx, row); err != nil {
		return judge.Answer{}, err
	}
	return decided.Answer, nil
}

// reuse answers a question from the verdict on record for it. When the record
// names another rule, or none — the candidate a person chose from a proposal,
// judged before it was a rule — the answer is recorded again for this one, so
// every rule's verdicts name it and a grant is traced to the rule it was
// minted under (§5). The copy keeps who judged and what they were put; nobody
// was asked again.
//
// It is recorded under a rule once. The newest answer on record may name
// another rule that asks the same question, so the rule's own row is looked
// for rather than read off that one: two rules with one sentence would
// otherwise copy a row for each other on every run.
func (s *Service) reuse(ctx context.Context, recorded *model.CredentialVerdict, row *model.CredentialVerdict) (judge.Answer, error) {
	answer := judge.Answer{Allow: recorded.Allow, Reason: recorded.Reason}
	if row.RuleID == "" || recorded.RuleID == row.RuleID {
		return answer, nil
	}
	named, err := s.store.QuestionRecordedUnder(ctx, recorded.ProjectID, recorded.SandboxID, row.SecretRequestID, recorded.QuestionDigest, row.RuleID)
	if err != nil || named {
		return answer, err
	}
	again := *recorded
	again.ID, again.CreatedAt = "", time.Time{}
	again.RuleID, again.SecretRequestID = row.RuleID, row.SecretRequestID
	again.LatencyMS = 0
	if err := s.store.CreateCredentialVerdict(ctx, &again); err != nil {
		return judge.Answer{}, err
	}
	return answer, nil
}

// ProposeRule has the project's judge discobox write the wider policy of a
// rule from one request's (§7). Jev writes nothing, so only a judge
// discobox can: a server that judges with Jev and lets its refusals stand has
// none, and neither has a project whose judge is not up yet. Each is an error
// saying so, and the caller then offers the narrowest level alone.
func (s *Service) ProposeRule(ctx context.Context, projectID string, job judge.ProposalJob) (judge.Proposal, error) {
	if err := job.Validate(); err != nil {
		return judge.Proposal{}, apperrors.NewStatusError(http.StatusBadRequest, "nothing can be proposed from this request: "+err.Error())
	}
	if s.leases == nil {
		return judge.Proposal{}, apperrors.NewStatusError(http.StatusServiceUnavailable, "this server cannot reach a judge")
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return judge.Proposal{}, apperrors.NotFound(err, "project not found")
	}
	judgeSandbox, err := s.readyJudge(ctx, project)
	if err != nil {
		return judge.Proposal{}, err
	}
	bound := judge.ReachWait + judge.Timeout + judgeRoutingGrace
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	reachCtx, cancelReach := context.WithTimeout(ctx, judge.ReachWait)
	lease, sandboxModel, err := s.leases.AcquireSandboxHTTPClientForServer(reachCtx, project.ID, judgeSandbox.ID, []string{poolagentauth.ScopeJudgeRun}, services.SandboxAction("judge"))
	cancelReach()
	if err != nil {
		return judge.Proposal{}, err
	}
	defer lease.Release()
	target, err := sandboxagentclient.TargetURL(lease.BaseURL, sandboxModel.ProjectID, sandboxModel.PoolID, sandboxModel.ID, "/judge/proposal")
	if err != nil {
		return judge.Proposal{}, err
	}
	jobBody := sandboxapi.JudgeProposalJob{Statements: job.Statements, Hosts: job.Hosts, Credential: optString(job.Credential)}
	if len(job.Definitions) > 0 {
		jobBody.Definitions = sandboxapi.NewOptJudgeProposalJobDefinitions(job.Definitions)
	}
	// Through the pointer, for the reason putSandbox marshals its job so.
	body, err := json.Marshal(&jobBody)
	if err != nil {
		return judge.Proposal{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return judge.Proposal{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := sandboxagentclient.HTTPClient(lease).Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return judge.Proposal{}, apperrors.NewStatusError(http.StatusGatewayTimeout,
				fmt.Sprintf("the project's judge did not write a proposal inside %s", bound.Round(time.Second)))
		}
		return judge.Proposal{}, apperrors.NewStatusError(http.StatusBadGateway,
			fmt.Sprintf("the project's judge could not be reached: %v", err))
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return judge.Proposal{}, judgeError(response)
	}
	// Decoded the way the judge runtime decoded the model's own output: the
	// proposal crossed two hops as JSON, and what it may hold is spelled out
	// in one place.
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(judge.MaxOutput)+1))
	if err != nil {
		return judge.Proposal{}, apperrors.NewStatusError(http.StatusBadGateway,
			fmt.Sprintf("the project's judge answered with something unreadable: %v", err))
	}
	proposal, err := judge.DecodeProposal(raw)
	if err != nil {
		return judge.Proposal{}, apperrors.NewStatusError(http.StatusBadGateway,
			fmt.Sprintf("the project's judge answered with something that is not a proposal: %v", err))
	}
	return proposal, nil
}
