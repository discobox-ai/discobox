package judges

import (
	"context"
	"errors"
	"net/http"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// JudgeCommand asks the judge of the project that owns the asking pool whether
// a command one of its discoboxes is about to run carries out the use it
// names, before the pool mints anything for it (ADR 26-09-22-838 §3). It is
// the judge `discobox-access run` used to ask inside the sandbox, moved to
// where the discobox cannot choose the executable, the model, or the answer.
//
// The pool names the discobox and the use. What the use approves, the
// credential behind it and the host it may go to are read here from the live
// grant. The argv, its input and where the discobox says it runs are evidence
// the discobox wrote.
//
// Every way of not answering is a refusal, as for a request. A server that
// does not judge commands says so in a way the pool recognizes, and the pool
// then mints without asking (ADR 26-10-02-054 §3). The answer is recorded as
// a command verdict before it goes back, so a pool never mints for a verdict
// that is not on record (ADR 0091).
func (s *Service) JudgeCommand(ctx context.Context, poolID string, ask services.CommandAsk) (judge.Answer, error) {
	if !s.judging.Commands {
		return judge.Answer{}, apperrors.NewStatusErrorOfKind(http.StatusServiceUnavailable,
			apperrors.KindJudgingDisabled, "this server does not judge commands")
	}
	if s.uses == nil || (s.jev == nil && s.leases == nil) {
		return judge.Answer{}, apperrors.NewStatusError(http.StatusServiceUnavailable, "this server cannot reach a judge")
	}
	pool, err := s.store.GetPoolByID(ctx, poolID)
	if err != nil {
		return judge.Answer{}, apperrors.NotFound(err, "pool not found")
	}
	project, err := s.store.GetProject(ctx, pool.ProjectID)
	if err != nil {
		return judge.Answer{}, apperrors.NotFound(err, "project not found")
	}
	var judgeSandbox *model.Sandbox
	if s.jev == nil {
		if judgeSandbox, err = s.readyJudge(ctx, project); err != nil {
			return judge.Answer{}, s.withoutAJudge(ctx, project, err)
		}
	}
	use, err := s.uses.ApprovedCredentialUse(ctx, poolID, ask.SandboxID, ask.UseID)
	if err != nil {
		return judge.Answer{}, err
	}
	job := judge.Job{
		Kind:       judge.KindCommand,
		Purpose:    use.Purpose,
		Host:       use.Host,
		Credential: use.Credential,
		Round:      1,
		Command:    ask.Command,
		Stdin:      ask.Stdin,
		Reported:   ask.Reported,
	}
	if err := job.Validate(); err != nil {
		return judge.Answer{}, apperrors.NewStatusError(http.StatusBadRequest, "the command cannot be judged: "+err.Error())
	}

	bound := s.judgeBound()
	judgeCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	decided, err := s.put(judgeCtx, project, judgeSandbox, job, bound)
	if err != nil {
		return judge.Answer{}, err
	}
	// A command is decided once: nothing stands for it, and there is nothing
	// further to show. A judge that asks anyway has not allowed it.
	decided.Standing = nil
	if decided.Need != nil {
		decided.Answer = judge.Answer{Reason: "the judge asked to be shown more, and a command has nothing more to show"}
	}
	// What follows the verdict has a deadline of its own, as for a request,
	// so a judge discobox that ran the judge's out leaves Jev's refusal
	// checked and recorded.
	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancelRecord()
	// Asked again after the verdict, as for a request: a grant revoked while
	// the judge read the command mints nothing.
	if _, err := s.uses.ApprovedCredentialUse(recordCtx, poolID, ask.SandboxID, ask.UseID); err != nil {
		return judge.Answer{}, err
	}

	prompt, err := judge.Prompt(job)
	if err != nil {
		return judge.Answer{}, err
	}
	row := &model.CredentialVerdict{
		ProjectID: project.ID,
		Kind:      model.CredentialVerdictKindCommand,
		Origin:    model.CredentialVerdictOriginJudge,
		SandboxID: ask.SandboxID,
		UseID:     ask.UseID,
		GrantID:   use.GrantID,
		Command:   job.Command,
		Round:     job.Round,
		Allow:     decided.Allow,
		Reason:    decided.Reason,
		Prompt:    prompt,
	}
	decided.stamp(row)
	if err := s.store.CreateCredentialVerdict(recordCtx, row); err != nil {
		return judge.Answer{}, err
	}
	return decided.Answer, nil
}

// withoutAJudge says the way out of a refusal for a project that cannot have a
// judge at all — no default harness, or one that runs no model. With commands
// judged by default, that is every credentialed command in the project, and
// what fixes it is not the discobox's to do (ADR 26-10-02-054 §2). A judge
// that is only not ready yet, or failed, is refused as it was.
func (s *Service) withoutAJudge(ctx context.Context, project *model.Project, refused error) error {
	wanted, _, err := s.wanted(ctx, project, false)
	if err != nil || wanted != nil {
		return refused
	}
	var status apperrors.StatusError
	if !errors.As(refused, &status) {
		return refused
	}
	status.Message += "; a credentialed command needs one, so configure a default harness for the project that runs a model, or have the server stop judging commands (judgeCommands: false)"
	return status
}
