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

// An approval rule's two readings, and the wording a person chooses one from
// (ADR 26-10-12-330).
//
// Rule jobs hang on neither judgeCommands nor judgeCredentials (§6): a rule is
// its own opt-in, and a job is refused only when there is nothing to ask. A
// project keeps a judge discobox while it has a rule switched on (wanted).
//
// Each question is answered once. The verdict records the digest of the job
// that was judged, and the same job asked again — another request from a
// discobox whose scope is already decided, the candidate a person chose from a
// proposal — is answered from the record. An edited rule is a different job.

// JudgeScope asks whether one discobox is among those a rule is for.
//
// With Jev, Jev's yes is a match and anything else is not, and nothing is put
// to the judge discobox (§2): most discoboxes do not match most rules, and a
// wrong "no match" costs only a person being asked, which is what happens
// today. A server with no Jev key asks its judge discobox.
func (s *Service) JudgeScope(ctx context.Context, projectID string, ask services.ScopeAsk) (judge.Answer, error) {
	row := &model.CredentialVerdict{
		Kind:            model.CredentialVerdictKindScope,
		SandboxID:       ask.SandboxID,
		RuleID:          ask.RuleID,
		SecretRequestID: ask.RequestID,
	}
	// A scope is decided once for a discobox, whatever it asks for.
	return s.judgeRuleJob(ctx, projectID, ask.Job(), row, "", func(ctx context.Context, project *model.Project, job judge.Job, bound time.Duration) (answered, error) {
		if s.jev != nil {
			return s.putJev(ctx, job, bound)
		}
		return s.putJudgeDiscobox(ctx, project, job, bound)
	})
}

// JudgeRule asks whether the uses a request asked for fall within a rule's.
// It goes the way every job does: Jev first when the server has a key, what
// Jev does not allow handled by jevUnsure, and the judge discobox otherwise
// (§5). The fallback is kept here and not for a scope because of how often
// each is asked: this runs only for a discobox already in scope, about a
// request somebody would otherwise have to read.
func (s *Service) JudgeRule(ctx context.Context, projectID string, ask services.RuleAsk) (judge.Answer, error) {
	row := &model.CredentialVerdict{
		Kind:            model.CredentialVerdictKindRule,
		SandboxID:       ask.SandboxID,
		RuleID:          ask.RuleID,
		SecretRequestID: ask.RequestID,
	}
	return s.judgeRuleJob(ctx, projectID, ask.Job(), row, ask.RequestID, func(ctx context.Context, project *model.Project, job judge.Job, bound time.Duration) (answered, error) {
		var judgeSandbox *model.Sandbox
		if s.jev == nil {
			var err error
			if judgeSandbox, err = s.readyJudge(ctx, project); err != nil {
				return answered{}, err
			}
		}
		return s.put(ctx, project, judgeSandbox, job, bound)
	})
}

// judgeRuleJob answers one of a rule's questions: from the record when the
// same job was already judged, and otherwise by asking and recording. row
// says what the verdict is about; requestID narrows the record to one
// request's, for a question asked per request.
//
// Every way of not answering is no verdict, and nothing is recorded for it,
// so the question is asked again later: an answer on record is never asked
// about again, and a judge that did not decide must not become one.
func (s *Service) judgeRuleJob(ctx context.Context, projectID string, job judge.Job, row *model.CredentialVerdict, requestID string,
	ask func(context.Context, *model.Project, judge.Job, time.Duration) (answered, error)) (judge.Answer, error) {
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
	recorded, err := s.store.AnsweredQuestion(ctx, projectID, row.Kind, row.SandboxID, requestID, digest, true)
	switch {
	case err == nil:
		return s.reuse(ctx, recorded, row, requestID)
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
	decided, err := ask(judgeCtx, project, job, bound)
	if err != nil {
		return judge.Answer{}, err
	}
	// An answer on record is reused for good, so only a decision is recorded.
	// Neither job has a body to be shown, so an ask for one decides nothing.
	// And where Jev's refusals go on to the judge discobox, a refusal that
	// comes back as Jev's alone is one the discobox did not answer — not up
	// yet, or out of time — so the one who decides has not: it is no verdict,
	// and the question is asked again.
	switch {
	case !decided.Decided():
		return judge.Answer{}, apperrors.NewStatusError(http.StatusBadGateway,
			"the judge asked to be shown something a rule's question does not have, which decides nothing")
	case row.Kind == model.CredentialVerdictKindRule && s.jevFallback && !decided.Allow && decided.sandbox == nil:
		return judge.Answer{}, apperrors.NewStatusError(http.StatusServiceUnavailable,
			"Jev did not allow it and the project's judge discobox, which decides what Jev does not, did not answer")
	}
	// Nothing stands for either job: each is its own question.
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
func (s *Service) reuse(ctx context.Context, recorded *model.CredentialVerdict, row *model.CredentialVerdict, requestID string) (judge.Answer, error) {
	answer := judge.Answer{Allow: recorded.Allow, Reason: recorded.Reason}
	if row.RuleID == "" || recorded.RuleID == row.RuleID {
		return answer, nil
	}
	named, err := s.store.QuestionRecordedUnder(ctx, recorded.ProjectID, recorded.Kind, recorded.SandboxID, requestID, recorded.QuestionDigest, row.RuleID)
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

// ProposeRule has the project's judge discobox write the wider scopes and uses
// of a rule from one request (§7). Jev writes nothing, so only a judge
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
	lease, sandboxModel, err := s.leases.AwaitSandboxHTTPClientForServer(reachCtx, project.ID, judgeSandbox.ID, []string{poolagentauth.ScopeJudgeRun})
	cancelReach()
	if err != nil {
		return judge.Proposal{}, err
	}
	defer lease.Release()
	target, err := sandboxagentclient.TargetURL(lease.BaseURL, sandboxModel.ProjectID, sandboxModel.PoolID, sandboxModel.ID, "/judge/proposal")
	if err != nil {
		return judge.Proposal{}, err
	}
	jobBody := sandboxapi.JudgeProposalJob{Uses: job.Uses, Hosts: job.Hosts, Credential: optString(job.Credential), Discobox: factSheetBody(job.Discobox)}
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

// factSheetBody is a fact sheet on the wire to the judge's agent.
func factSheetBody(sheet judge.Discobox) sandboxapi.JudgeDiscobox {
	body := sandboxapi.JudgeDiscobox{
		CreatedBy:    sandboxapi.JudgeDiscoboxCreatedBy(sheet.CreatedBy),
		Creator:      optString(sheet.Creator),
		Repository:   optString(sheet.Repository),
		Ref:          optString(sheet.Ref),
		OtherSources: sheet.OtherSources,
		Skills:       sheet.Skills,
		Harness:      optString(sheet.Harness),
		Platform:     optString(sheet.Platform),
	}
	if sheet.Prompt != nil {
		prompt := sandboxapi.JudgeInput{Content: sheet.Prompt.Content, Missing: optString(sheet.Prompt.Missing)}
		body.Prompt = sandboxapi.NewOptJudgeInput(prompt)
	}
	if sheet.PromptWrittenBy != "" {
		body.PromptWrittenBy = sandboxapi.NewOptJudgeDiscoboxPromptWrittenBy(sandboxapi.JudgeDiscoboxPromptWrittenBy(sheet.PromptWrittenBy))
	}
	return body
}
