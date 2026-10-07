package judges

import (
	"context"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// JudgeDelegation asks the project's judge whether the uses a discobox is
// about to hand on fall within the uses of the delegation grant it approves
// under (ADR 26-09-30-782 §3). It is the server's own question rather than a
// pool's: the secrets service asks it while approving, from the grant and the
// request it read.
//
// Every way of not answering is a refusal, the same as for a request: a server
// that does not judge, a project with no judge, a judge that cannot be reached.
// The answer is recorded as a delegation verdict against the delegation grant
// before it is returned, so the grant a discobox hands on can be traced to the
// delegation that allowed it.
func (s *Service) JudgeDelegation(ctx context.Context, projectID string, ask services.DelegationAsk) (judge.Answer, error) {
	// A delegation is judged with commands: it is put before anything is
	// minted, and it holds no request open (ADR 26-10-02-054 §1).
	if !s.judging.Commands {
		return judge.Answer{}, apperrors.NewStatusErrorOfKind(http.StatusServiceUnavailable,
			apperrors.KindJudgingDisabled, "this server does not judge commands, so a discobox hands nothing on")
	}
	if s.jev == nil && s.leases == nil {
		return judge.Answer{}, apperrors.NewStatusError(http.StatusServiceUnavailable, "this server cannot reach a judge")
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return judge.Answer{}, apperrors.NotFound(err, "project not found")
	}
	var judgeSandbox *model.Sandbox
	if s.jev == nil {
		if judgeSandbox, err = s.readyJudge(ctx, project); err != nil {
			return judge.Answer{}, err
		}
	}
	job := judge.Job{
		Kind:       judge.KindDelegation,
		Purpose:    strings.Join(ask.Delegated, "\n"),
		Host:       strings.Join(ask.Hosts, ", "),
		Credential: ask.Credential,
		Round:      1,
		Uses:       ask.Uses,
	}
	if err := job.Validate(); err != nil {
		return judge.Answer{}, apperrors.NewStatusError(http.StatusBadRequest, "a delegation cannot be judged: "+err.Error())
	}

	bound := min(s.judgeBound(), services.DelegationBound)
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	decided, err := s.put(ctx, project, judgeSandbox, job, bound)
	if err != nil {
		return judge.Answer{}, err
	}
	// Nothing stands for a delegation: each approval is its own question.
	decided.Standing = nil

	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancelRecord()
	prompt, err := judge.Prompt(job)
	if err != nil {
		return judge.Answer{}, err
	}
	row := &model.CredentialVerdict{
		ProjectID:       project.ID,
		Kind:            model.CredentialVerdictKindDelegation,
		Origin:          model.CredentialVerdictOriginJudge,
		SandboxID:       ask.ApproverID,
		GrantID:         ask.DelegationGrantID,
		SecretRequestID: ask.RequestID,
		ForSandboxID:    ask.ForSandboxID,
		Round:           job.Round,
		Allow:           decided.Allow,
		Need:            decided.Need,
		Reason:          decided.Reason,
		Prompt:          prompt,
	}
	decided.stamp(row)
	if err := s.store.CreateCredentialVerdict(recordCtx, row); err != nil {
		return judge.Answer{}, err
	}
	return decided.Answer, nil
}
