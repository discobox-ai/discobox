package judges

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-faster/jx"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/judge/jev"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandboxagentclient"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// Routing a job to the judge (ADR 26-09-22-838 §2).
//
// A pool asks the control plane, and the control plane forwards to the pool
// hosting the project's judge, over the channel it already uses to create and
// start discoboxes there. Pools never call each other: they sit behind NAT, in
// clouds, and inside VMs, and the only thing every pool can reach is here.

// Leases is the sandbox service's half of reaching a discobox's agent. It is
// named here rather than taken whole because this package makes one call.
type Leases interface {
	AwaitSandboxHTTPClientForServer(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error)
}

// SetLeases installs it. A judge that cannot be reached is a judge that
// refuses, so this is required before Judge answers anything.
func (s *Service) SetLeases(leases Leases) { s.leases = leases }

// Uses is the secrets service's half of saying what an approved use allows.
// It is named here for the same reason Leases is: this package asks it what a
// request's use allows, and what a command's does.
type Uses interface {
	ApprovedUse(ctx context.Context, poolID, sandboxID, useID, host string) (services.ApprovedUse, error)
	ApprovedCredentialUse(ctx context.Context, poolID, sandboxID, useID string) (services.ApprovedUse, error)
}

// SetUses installs it. A judge with no question to put has nothing to answer,
// so this is required before Judge answers anything.
func (s *Service) SetUses(uses Uses) { s.uses = uses }

// Judge puts one job to the judge of the project that owns the asking pool.
//
// Every refusal here is the same answer: no verdict. A project with no judge, a
// judge that will not come up, a pool that cannot be reached — none of them
// allow anything, and the reason travels back so the pool can say why the
// credential its discobox asked for is not coming (ADR 26-09-22-838 §1).
func (s *Service) Judge(ctx context.Context, poolID string, ask services.JudgeAsk) (judge.Answer, error) {
	// A pool that asks a server which does not judge is answered before
	// anything is looked up. Nothing should be asking — the pool is told
	// whether to — so this is the backstop, not the path.
	if !s.judging.Requests {
		// Said in a way a program can recognize, because a pool has to tell it
		// apart from a judge that failed: one means stop asking, the other
		// means no credential goes out (ADR 26-09-22-838 §4).
		return judge.Answer{}, apperrors.NewStatusErrorOfKind(http.StatusServiceUnavailable,
			apperrors.KindJudgingDisabled, "this server does not judge credential-bearing requests")
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
	// Jev is always there to ask, so only a judge discobox is looked for.
	var judgeSandbox *model.Sandbox
	if s.jev == nil {
		if judgeSandbox, err = s.readyJudge(ctx, project); err != nil {
			return judge.Answer{}, err
		}
	}

	// The question is composed once there is something that could answer it:
	// reading a use out of the grants is work, and a project with no judge
	// refuses whatever the use turns out to say. That holds for an allow the
	// judge let stand as well: a project whose judge is gone or failed has
	// nobody standing behind it, and refuses the same way.
	job, use, err := s.job(ctx, poolID, ask)
	if err != nil {
		return judge.Answer{}, err
	}
	// An allow the judge already let stand may answer it (ADR 26-09-25-428
	// §3). Reading the use above is also what checked it is still live, which
	// a standing allow needs on every request as much as a fresh one does.
	if standing, ok, err := s.standing(ctx, project.ID, ask, job, use); err != nil || ok {
		return standing, err
	}

	// One ask is bounded here as well as at the judge (ADR 26-09-22-838 §2): a caller
	// that passed no deadline must not be able to hold this goroutine, the
	// lease, and one of the judge's runs for as long as the judge is willing
	// to think. The sandbox agent bounds how long an ask waits for a run, and
	// the run itself, inside this.
	//
	// A pool says how long it will wait, and that is a bound too. Its rounds
	// share one deadline, so a later round comes with less time than this
	// hop's own; answered inside what the pool has left, the refusal arrives
	// with its sentence rather than as the pool giving up on a silence.
	bound := s.judgeBound()
	if ask.Timeout > 0 {
		left := ask.Timeout - judgeReplyMargin
		if left <= 0 {
			return judge.Answer{}, apperrors.NewStatusError(http.StatusGatewayTimeout,
				"the request had no time left to be judged in")
		}
		bound = min(bound, left)
	}
	// A later round is Jev's only when Jev asked for it (ADR 26-10-07-937
	// §2). One the judge discobox asked for, after taking the job over from
	// a Jev refusal, is the discobox's, and so is one whose asker cannot be
	// found: Jev answering a body it never asked for must not decide it.
	jevs, err := s.jevsRound(ctx, project.ID, ask, job)
	if err != nil {
		return judge.Answer{}, err
	}
	judgeCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	var decided answered
	if jevs {
		decided, err = s.put(judgeCtx, project, judgeSandbox, job, bound)
	} else {
		decided, err = s.putJudgeDiscobox(judgeCtx, project, job, bound)
	}
	if err != nil {
		return judge.Answer{}, err
	}
	// What follows the verdict has a deadline of its own rather than the
	// judge's, so a judge discobox that ran the judge's deadline out leaves
	// Jev's refusal checked and recorded rather than lost to the expired
	// context (ADR 26-10-07-937 §4). It is bounded all the same, and the
	// pool's own wait leaves judgeReplyMargin for it.
	afterCtx, cancelAfter := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancelAfter()
	// Asked again after the verdict, because a verdict takes a while and a
	// grant can be revoked inside it (ADR 26-09-22-838 §4). The check is the same one
	// the question was built from, so what it rules out is a use that stopped
	// being approved while a model was reading the request it authorized.
	//
	// Before the answer is recorded, not after: an answer about a use that no
	// longer exists is not what the request was refused on, and a row saying
	// allow for it would contradict the proxy's own record of the refusal.
	// Like an ask that got no answer, it leaves the proxy's blocked row alone.
	if _, err := s.uses.ApprovedUse(afterCtx, poolID, ask.SandboxID, ask.UseID, judgedHost(ask.Request)); err != nil {
		return judge.Answer{}, err
	}
	standingUntil := s.admit(afterCtx, job, &decided.Answer, time.Now())
	// Recorded before the answer goes back, and gating it: an answer with no
	// record of it is no verdict (ADR 26-09-22-838 §§4, 8). The model has
	// already been paid to give it, so an answer that arrived just inside the
	// ask's deadline is not lost to it.
	if err := s.record(afterCtx, project.ID, ask, use, job, decided, standingUntil); err != nil {
		return judge.Answer{}, err
	}
	return decided.Answer, nil
}

// jevsRound reports whether this round of an ask is Jev's to answer first.
// Every round is on a server that does not send Jev's refusals on, and so is
// the first round on one that does. A later round there is Jev's only when the
// round before was Jev asking for the body; one the judge discobox asked for
// is the discobox's, which answers that only after Jev refused.
//
// A pool asks again with the body under the same discobox and use, at the
// next round, within one exchange's deadline; nothing else ties the rounds of
// one ask together. So the round before is a recorded ask for a body in that
// round, within that time, about the same request: its method and URL, and its
// body's media type and length. It is read from the primary, since it was
// written there moments ago and a replica may not have it yet. Even so it
// fails closed: a later round whose round before cannot be found as Jev's ask —
// a lagging read, a request that reads differently between rounds, a round
// outside the window — goes to the judge discobox, which costs a call and
// never lets Jev allow alone what it did not ask to see. Where two asks match
// and either was the discobox's, the discobox decides.
func (s *Service) jevsRound(ctx context.Context, projectID string, ask services.JudgeAsk, job judge.Job) (bool, error) {
	if s.jev == nil || !s.jevFallback || job.Round <= 1 {
		return true, nil
	}
	rows, err := s.store.BodyAsks(ctx, projectID, ask.SandboxID, ask.UseID, job.Round-1, time.Now().Add(-s.judgeBound()))
	if err != nil {
		return false, err
	}
	jevAsked := false
	for i := range rows {
		if !sameRequest(rows[i].Request, job.Request) {
			continue
		}
		if rows[i].JudgeSandboxID != "" || rows[i].Model == "" {
			return false, nil
		}
		jevAsked = true
	}
	return jevAsked, nil
}

// sameRequest reports whether an earlier round's evidence is about the same
// request as this round's: the same method and URL, and a body described the
// same way, which showing it does not change.
func sameRequest(earlier, now *judge.Request) bool {
	if earlier == nil || now == nil || earlier.Method != now.Method || earlier.URL != now.URL {
		return false
	}
	if (earlier.Body == nil) != (now.Body == nil) {
		return false
	}
	return earlier.Body == nil || (earlier.Body.MediaType == now.Body.MediaType && earlier.Body.Length == now.Body.Length)
}

// judgeBound is how long one ask may take on this hop, before a pool's own
// deadline is applied. A judge discobox may first have to be reached; Jev is
// an HTTP call away and gets the judge's own time alone, unless what it
// refuses goes on to a judge discobox, which may have to be reached too.
func (s *Service) judgeBound() time.Duration {
	if s.jev != nil && !s.jevFallback {
		return judge.Timeout
	}
	return judge.ReachWait + judge.Timeout + judgeRoutingGrace
}

// answered is one judge's answer to one job, and which judge gave it: what a
// verdict records about who decided.
type answered struct {
	judge.Answer
	latency time.Duration
	// sandbox is the judge discobox that answered, when one did.
	sandbox *model.Sandbox
	// model, probabilities and input are Jev's, when Jev was asked: what it
	// said and what it was sent, whether it decided or refused and a judge
	// discobox decided after it.
	model         string
	probabilities map[string]float64
	input         json.RawMessage
}

// stamp writes who answered, and how long it took, onto the verdict that
// records the answer. A verdict a judge discobox decided after Jev refused
// names both, and its prompt version is the discobox's: it is whose words
// decided.
func (a answered) stamp(row *model.CredentialVerdict) {
	row.LatencyMS = a.latency.Milliseconds()
	if a.model != "" {
		// Jev, asked no role and given no system prompt: its questions and
		// thresholds are what the version names (ADR 26-10-01-324 §6).
		row.Model = a.model
		row.Probabilities = a.probabilities
		row.JevInput = a.input
		row.PromptVersion = jev.QuestionsVersion
	}
	if a.sandbox == nil {
		return
	}
	row.Role = judge.Role
	row.PromptVersion = judge.PromptVersion
	row.JudgeSandboxID = a.sandbox.ID
	row.Image = a.sandbox.Image
	row.ImageDigest = a.sandbox.ImageDigest
	if a.sandbox.HarnessConfigID != nil {
		row.HarnessConfigID = *a.sandbox.HarnessConfigID
	}
}

// put asks the server's judge one job: Jev, when the server judges with it,
// and otherwise the project's judge discobox. ctx bounds the whole exchange;
// bound is what it was bounded by, for the refusal that says so.
//
// A server that sends Jev's refusals on (jevUnsure: harness) asks the
// project's judge discobox about every job Jev refused, whatever refused it —
// a clear no, a no Jev could not tell, or a hazard — and takes its answer
// (ADR 26-10-07-937). Jev decides its allows alone, and an ask to be shown a
// body is not a refusal: it goes back to be answered with the body. A later
// round reaches put only when it is Jev's own (jevsRound); Judge puts any
// other straight to the judge discobox. Jev was going to refuse what goes on,
// so a judge discobox that cannot be had leaves Jev's refusal standing rather
// than no verdict at all.
func (s *Service) put(ctx context.Context, project *model.Project, judgeSandbox *model.Sandbox, job judge.Job, bound time.Duration) (answered, error) {
	if s.jev == nil {
		return s.putSandbox(ctx, project, judgeSandbox, job, bound)
	}
	jevAnswer, err := s.putJev(ctx, job, bound)
	if err != nil || !s.jevFallback || !jevAnswer.Decided() || jevAnswer.Allow {
		return jevAnswer, err
	}
	if s.leases == nil {
		return jevAnswer, nil
	}
	decided, err := s.putJudgeDiscobox(ctx, project, job, bound)
	if err != nil {
		s.logger.WarnContext(ctx, "Jev refused and the project's judge discobox did not answer, so Jev's refusal stands",
			"projectId", project.ID, "error", err)
		return jevAnswer, nil
	}
	decided.latency += jevAnswer.latency
	decided.model, decided.probabilities, decided.input = jevAnswer.model, jevAnswer.probabilities, jevAnswer.input
	return decided, nil
}

// putJudgeDiscobox asks the project's judge discobox on a server that judges
// with Jev: one Jev refused, or one that took the job over in an earlier
// round. It has to be brought up or found first, inside the same bound.
func (s *Service) putJudgeDiscobox(ctx context.Context, project *model.Project, job judge.Job, bound time.Duration) (answered, error) {
	if s.leases == nil {
		return answered{}, apperrors.NewStatusError(http.StatusServiceUnavailable, "this server cannot reach a judge")
	}
	judgeSandbox, err := s.readyJudge(ctx, project)
	if err != nil {
		return answered{}, err
	}
	return s.putSandbox(ctx, project, judgeSandbox, job, bound)
}

// putJev asks Jev. Every way it fails to answer is no verdict, said in a
// sentence that is safe to give the discobox that asked; what Jev itself said
// goes to the operator's log, since it can quote the request it was sent.
func (s *Service) putJev(ctx context.Context, job judge.Job, bound time.Duration) (answered, error) {
	start := time.Now()
	verdict, err := s.jev.Judge(ctx, job)
	if err == nil {
		return answered{Answer: verdict.Answer, latency: time.Since(start), model: verdict.Model,
			probabilities: verdict.Probabilities, input: verdict.Input}, nil
	}
	var status *jev.StatusError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return answered{}, apperrors.NewStatusError(http.StatusGatewayTimeout,
			fmt.Sprintf("Jev did not answer inside the %s the request had", bound.Round(time.Second)))
	case errors.As(err, &status) && status.Unauthorized():
		s.logger.ErrorContext(ctx, "Jev refused this server's API key, so a verdict was refused", "error", err)
		return answered{}, apperrors.NewStatusError(http.StatusServiceUnavailable,
			"this server's judge could not be asked; the server's log says why")
	case errors.As(err, &status) && status.Busy():
		s.logger.WarnContext(ctx, "Jev was too busy to answer, so a verdict was refused", "error", err)
		return answered{}, apperrors.NewStatusError(http.StatusTooManyRequests, "Jev was too busy to judge the request")
	default:
		s.logger.WarnContext(ctx, "Jev did not answer, so a verdict was refused", "error", err)
		return answered{}, apperrors.NewStatusError(http.StatusBadGateway, "Jev could not judge the request; the server's log says why")
	}
}

// readyJudge is the project's judge when it can answer, and otherwise the
// refusal that says why: none yet, or one that could not be brought up.
func (s *Service) readyJudge(ctx context.Context, project *model.Project) (*model.Sandbox, error) {
	judgeSandbox, err := s.judge(ctx, project)
	if err != nil {
		return nil, err
	}
	if judgeSandbox == nil {
		// Read-only: this is a verdict being asked for, not the convergence
		// deciding.
		_, why, err := s.wanted(ctx, project, false)
		if err != nil {
			return nil, err
		}
		if why == "" {
			why = "the project's judge is not ready yet"
		}
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "this project has no judge: "+why)
	}
	// A judge that could not be brought up is a refusal with a stable sentence,
	// and the reason goes to the log rather than back down the wire.
	//
	// The judge is in no listing, so this is the only place its failure is
	// mentioned at all — but it travels to the pool, and from there to the
	// discobox that asked (ADR 26-09-22-838 §4: the reason is what a discobox learns).
	// A sandbox's own reconcile error is written for whoever runs the server:
	// it carries pool host paths, image references and whatever a provider's
	// API said. That is an operator's to read, in the operator's log.
	if judgeSandbox.State == model.SandboxStateFailed {
		detail := ""
		if judgeSandbox.ErrorMessage != nil {
			detail = strings.TrimSpace(*judgeSandbox.ErrorMessage)
		}
		s.logger.WarnContext(ctx, "the project's judge could not be brought up, so a verdict was refused",
			"projectId", project.ID, "sandboxId", judgeSandbox.ID, "poolId", judgeSandbox.PoolID, "error", detail)
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable,
			"this project's judge could not be brought up; the server's log says why")
	}

	return judgeSandbox, nil
}

// putSandbox asks the project's judge discobox one job and returns its answer
// and how long the round trip took, bringing up a judge that is about to be
// reachable first.
func (s *Service) putSandbox(ctx context.Context, project *model.Project, judgeSandbox *model.Sandbox, job judge.Job, bound time.Duration) (answered, error) {
	// The round trip starts here, before the judge is reached: bringing up a
	// stopped judge is part of how long it took to answer.
	start := time.Now()
	// A judge that is about to be reachable — its pool not yet heard from
	// since this server started, or its host still coming back — is waited on
	// rather than refused, for a bound of its own. Every deadline on the
	// exchange allows for it on top of the judge's own time, so a first round
	// that waits the whole of it still leaves the judge all of judge.Timeout.
	reachCtx, cancelReach := context.WithTimeout(ctx, judge.ReachWait)
	lease, sandboxModel, err := s.leases.AwaitSandboxHTTPClientForServer(reachCtx, project.ID, judgeSandbox.ID, []string{poolagentauth.ScopeJudgeRun})
	cancelReach()
	if err != nil {
		return answered{}, err
	}
	defer lease.Release()

	target, err := sandboxagentclient.TargetURL(lease.BaseURL, sandboxModel.ProjectID, sandboxModel.PoolID, sandboxModel.ID, "/judge")
	if err != nil {
		return answered{}, err
	}
	// Marshaled through the pointer, which is what reaches the generated
	// MarshalJSON. By value, encoding/json walks the struct itself and asks
	// each unset optional field to marshal — and an unset one writes nothing,
	// which fails the whole encode. The generated encoder is the only one that
	// knows to leave an unset field out.
	jobBody := judgeJobBody(job)
	body, err := json.Marshal(&jobBody)
	if err != nil {
		return answered{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return answered{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := sandboxagentclient.HTTPClient(lease).Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return answered{}, apperrors.NewStatusError(http.StatusGatewayTimeout,
				fmt.Sprintf("the project's judge did not answer inside the %s the request had", bound.Round(time.Second)))
		}
		return answered{}, apperrors.NewStatusError(http.StatusBadGateway,
			fmt.Sprintf("the project's judge could not be reached: %v", err))
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return answered{}, judgeError(response)
	}
	var reply sandboxapi.JudgeAnswer
	if err := json.NewDecoder(io.LimitReader(response.Body, int64(judge.MaxOutput))).Decode(&reply); err != nil {
		return answered{}, apperrors.NewStatusError(http.StatusBadGateway,
			fmt.Sprintf("the project's judge answered with something unreadable: %v", err))
	}
	return answered{Answer: answer(reply), latency: time.Since(start), sandbox: judgeSandbox}, nil
}

// recordTimeout bounds writing one verdict once the judge has answered.
const recordTimeout = 10 * time.Second

// record persists one answer as a request verdict. The row is written whatever
// the answer — an allow, a refusal, or an ask to be shown the body — because
// each is the judge's decision about a request that was held while it was
// made. The judge is read off whatever answered: the discobox, so a verdict
// names the harness and the image that produced it even after the project's
// judge is replaced, or Jev's model and what it said. An allow admitted to
// stand carries its route and when it lapses, which is what makes the row the
// standing allow.
func (s *Service) record(ctx context.Context, projectID string, ask services.JudgeAsk, use services.ApprovedUse, job judge.Job, decided answered, standingUntil *time.Time) error {
	prompt, err := judge.Prompt(job)
	if err != nil {
		return err
	}
	row := &model.CredentialVerdict{
		ProjectID: projectID,
		Kind:      model.CredentialVerdictKindRequest,
		Origin:    model.CredentialVerdictOriginJudge,
		SandboxID: ask.SandboxID,
		UseID:     ask.UseID,
		GrantID:   use.GrantID,
		Command:   job.Command,
		Request:   job.Request,
		Round:     job.Round,
		Allow:     decided.Allow,
		Need:      decided.Need,
		Reason:    decided.Reason,
		Prompt:    prompt,
	}
	decided.stamp(row)
	if decided.Standing != nil && standingUntil != nil {
		row.StandingRoute = decided.Standing.Route
		row.StandingUntil = standingUntil
	}
	return s.store.CreateCredentialVerdict(ctx, row)
}

// admit decides whether the allow the judge asked to let stand does, and until
// when (ADR 26-09-25-428 §2). The judge proposes a route; whether it was
// derived from this request's evidence, and how long it may last, are this
// side's. A route that fails is dropped from the answer and the allow stays:
// the request was allowed, and only the shortcut for the next one is refused.
func (s *Service) admit(ctx context.Context, job judge.Job, decided *judge.Answer, now time.Time) *time.Time {
	if decided.Standing == nil {
		return nil
	}
	if !decided.Allow {
		decided.Standing = nil
		return nil
	}
	if _, err := job.Admits(*decided.Standing); err != nil {
		s.logger.InfoContext(ctx, "the judge's standing route was not admitted", "route", decided.Standing.Route, "reason", err)
		decided.Standing = nil
		return nil
	}
	until := now.Add(decided.Standing.Duration())
	return &until
}

// standing answers an ask that an allow already stands for, without asking the
// judge (ADR 26-09-25-428 §3). It reports false for an ask none covers, which
// goes to the judge like any other.
//
// An allow covers an ask only when it was granted to the same discobox, under
// the same use of the same grant, against the same approved sentence, for a
// request to the same host, and its route matches this request. The use was
// checked live when the question was composed, so a revoked grant or a lapsed
// activation has already refused before anything here is read.
//
// A covered request is recorded like any other, naming the verdict that
// decided it, before the answer goes back: an allow with no record of it is no
// verdict, standing or not.
func (s *Service) standing(ctx context.Context, projectID string, ask services.JudgeAsk, job judge.Job, use services.ApprovedUse) (judge.Answer, bool, error) {
	if job.Round != 1 {
		// A later round is the judge asking to be shown the body, and an
		// allow that stands never needed one.
		return judge.Answer{}, false, nil
	}
	if job.Request.OperationInBody() != "" {
		// A route matches a method and a path, and says nothing about a
		// body. A request whose operation is in its body — a push, whatever
		// route an earlier allow named — is read every time, or nothing read
		// it (ADR 26-09-26-240 §2).
		return judge.Answer{}, false, nil
	}
	start := time.Now()
	rows, err := s.store.StandingVerdicts(ctx, projectID, ask.SandboxID, ask.UseID, start)
	if err != nil {
		return judge.Answer{}, false, err
	}
	origin := standingOrigin(job.Request)
	for i := range rows {
		granted := &rows[i]
		if !covers(granted, job, use, origin) {
			continue
		}
		row := &model.CredentialVerdict{
			ProjectID:         projectID,
			Kind:              model.CredentialVerdictKindRequest,
			Origin:            model.CredentialVerdictOriginJudge,
			SandboxID:         ask.SandboxID,
			UseID:             ask.UseID,
			GrantID:           use.GrantID,
			Command:           job.Command,
			Request:           job.Request,
			Round:             job.Round,
			Allow:             true,
			Reason:            granted.Reason,
			PromptVersion:     granted.PromptVersion,
			LatencyMS:         time.Since(start).Milliseconds(),
			JudgeSandboxID:    granted.JudgeSandboxID,
			HarnessConfigID:   granted.HarnessConfigID,
			Image:             granted.Image,
			ImageDigest:       granted.ImageDigest,
			Model:             granted.Model,
			StandingVerdictID: granted.ID,
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		err := s.store.CreateCredentialVerdict(recordCtx, row)
		cancel()
		if err != nil {
			return judge.Answer{}, false, err
		}
		return judge.Answer{Allow: true, Reason: granted.Reason}, true, nil
	}
	return judge.Answer{}, false, nil
}

// covers reports whether one standing allow covers this request. The grant
// and the sentence are compared as well as the use ID, because a standing
// allow was the judge's reading of the purpose it was shown, and a use that
// now approves something else has not been read by anyone.
func covers(granted *model.CredentialVerdict, job judge.Job, use services.ApprovedUse, origin string) bool {
	if granted.GrantID != use.GrantID || origin == "" || standingOrigin(granted.Request) != origin {
		return false
	}
	var asked judge.Job
	if err := json.Unmarshal([]byte(granted.Prompt), &asked); err != nil {
		return false
	}
	if asked.Purpose != job.Purpose || asked.Host != job.Host || asked.Credential != job.Credential {
		return false
	}
	route, err := judge.ParseRoute(granted.StandingRoute)
	if err != nil {
		return false
	}
	return route.Matches(job.Request.Method, job.Request.URL)
}

// job is the question this server puts, built from what the pool sent and what
// the control plane knows, and the use it was built from. The pool names the
// discobox and the use; the sentence that use approves, the credential behind
// it and the host it is approved for are read here, so a pool cannot widen its
// own question (ADR 26-09-22-838 §4).
func (s *Service) job(ctx context.Context, poolID string, ask services.JudgeAsk) (judge.Job, services.ApprovedUse, error) {
	if ask.Request == nil {
		return judge.Job{}, services.ApprovedUse{}, apperrors.NewStatusError(http.StatusBadRequest, "a request to judge is required")
	}
	use, err := s.uses.ApprovedUse(ctx, poolID, ask.SandboxID, ask.UseID, judgedHost(ask.Request))
	if err != nil {
		return judge.Job{}, services.ApprovedUse{}, err
	}
	job := judge.Job{
		Kind:       judge.KindRequest,
		Purpose:    use.Purpose,
		Host:       use.Host,
		Credential: use.Credential,
		Round:      ask.Round,
		Command:    ask.Command,
		Request:    ask.Request,
		// Read here, from what the pool says it recognized, and never taken
		// from the pool: the words about a protocol or an API are the trusted
		// side's (ADR 26-09-26-240 §4).
		Guidance: judge.GuidanceFor(ask.Request),
	}
	if err := job.Validate(); err != nil {
		return judge.Job{}, services.ApprovedUse{}, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	return job, use, nil
}

// judgedHost is where the request the pool observed is going, which is what
// the use has to cover. It is read off the URL rather than taken as a field of
// its own: the destination and the evidence must be the same destination.
func judgedHost(request *judge.Request) string {
	if request == nil {
		return ""
	}
	target, err := url.Parse(request.URL)
	if err != nil || target.Host == "" {
		return ""
	}
	return hostscope.Normalize(target.Host)
}

// standingOrigin is where a request went, as a standing allow compares it: the
// scheme and the authority with its port, not the normalized host a use is
// scoped by. Normalizing drops the port, and the judge read a request to one
// service; another port, or plain HTTP, on the same name is another one
// (ADR 26-09-25-428 §2). Ports are compared as written, so an explicit
// default port is a different origin: a miss is a request judged, which is the
// safe way to be wrong.
func standingOrigin(request *judge.Request) string {
	if request == nil {
		return ""
	}
	target, err := url.Parse(request.URL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return ""
	}
	return strings.ToLower(target.Scheme) + "://" + strings.ToLower(target.Host)
}

// judgeRoutingGrace is what this hop allows on top of the judge's own bound
// and the wait to reach it: the time to start a stopped judge and read the
// answer back.
// It is generous on purpose — the deadline here is a backstop against a caller
// with none, not the one that should normally fire.
const judgeRoutingGrace = 30 * time.Second

// judgeReplyMargin is what this hop leaves of a pool's own deadline for the
// answer to travel back in, so the pool reads it rather than timing out first.
const judgeReplyMargin = 5 * time.Second

// optString is a field the wire leaves out when it says nothing.
func optString(value string) sandboxapi.OptString {
	if value == "" {
		return sandboxapi.OptString{}
	}
	return sandboxapi.NewOptString(value)
}

// judgeJobBody is the job on the wire to the judge's agent.
func judgeJobBody(job judge.Job) sandboxapi.JudgeJob {
	body := sandboxapi.JudgeJob{
		Kind:     sandboxapi.JudgeJobKind(job.Kind),
		Purpose:  job.Purpose,
		Host:     job.Host,
		Round:    int64(job.Round),
		Command:  job.Command,
		Guidance: job.Guidance,
		Uses:     job.Uses,
	}
	if job.Credential != "" {
		body.Credential = sandboxapi.NewOptString(job.Credential)
	}
	if in := job.Stdin; in != nil {
		stdin := sandboxapi.JudgeInput{Content: in.Content}
		if in.Missing != "" {
			stdin.Missing = sandboxapi.NewOptString(in.Missing)
		}
		body.Stdin = sandboxapi.NewOptJudgeInput(stdin)
	}
	if r := job.Reported; r != nil {
		body.Reported = sandboxapi.NewOptJudgeReported(sandboxapi.JudgeReported{
			WorkingDirectory: optString(r.WorkingDirectory),
			RepositoryRoot:   optString(r.RepositoryRoot),
			RefCommit:        optString(r.RefCommit),
			RefSubject:       optString(r.RefSubject),
		})
	}
	if job.Request == nil {
		return body
	}
	evidence := sandboxapi.JudgeRequestEvidence{Method: job.Request.Method, URL: job.Request.URL}
	if len(job.Request.Headers) > 0 {
		evidence.Headers = sandboxapi.NewOptJudgeRequestEvidenceHeaders(job.Request.Headers)
	}
	evidence.Protocol = recognitionBody(job.Request.Protocol)
	evidence.Endpoint = recognitionBody(job.Request.Endpoint)
	if body := job.Request.Body; body != nil {
		requestBody := sandboxapi.JudgeRequestBody{Length: sandboxapi.NewOptInt64(body.Length)}
		if body.MediaType != "" {
			requestBody.MediaType = sandboxapi.NewOptString(body.MediaType)
		}
		requestBody.Parser = recognitionBody(body.Parser)
		if len(body.Metadata) > 0 {
			var fields map[string]json.RawMessage
			// Validate has already held it to one JSON object.
			if err := json.Unmarshal(body.Metadata, &fields); err == nil {
				metadata := make(sandboxapi.JudgeRequestBodyMetadata, len(fields))
				for key, value := range fields {
					metadata[key] = jx.Raw(value)
				}
				requestBody.Metadata = sandboxapi.NewOptJudgeRequestBodyMetadata(metadata)
			}
		}
		if body.ParseError != "" {
			requestBody.ParseError = sandboxapi.NewOptString(body.ParseError)
		}
		// Present, even empty, once the judge asked: an empty body shown is
		// not the same as a body not yet shown.
		if body.Content != nil {
			requestBody.Content = sandboxapi.NewOptString(*body.Content)
		}
		if body.Missing != "" {
			requestBody.Missing = sandboxapi.NewOptString(body.Missing)
		}
		evidence.Body = sandboxapi.NewOptJudgeRequestBody(requestBody)
	}
	body.Request = sandboxapi.NewOptJudgeRequestEvidence(evidence)
	return body
}

// recognitionBody is a recognition on the wire to the judge's agent.
func recognitionBody(recognized *judge.Recognition) sandboxapi.OptJudgeRecognition {
	if recognized == nil {
		return sandboxapi.OptJudgeRecognition{}
	}
	return sandboxapi.NewOptJudgeRecognition(sandboxapi.JudgeRecognition{
		Name: recognized.Name, Version: int64(recognized.Version),
	})
}

// answer is what the judge said, in the words this server passes on.
func answer(answered sandboxapi.JudgeAnswer) judge.Answer {
	out := judge.Answer{Reason: answered.Reason, Allow: answered.Allow.Or(false)}
	if need, ok := answered.Need.Get(); ok {
		out.Allow = false
		out.Need = &judge.Need{Body: need.Body, Bytes: int(need.Bytes.Or(0))}
		return out
	}
	if standing, ok := answered.Standing.Get(); ok && out.Allow {
		out.Standing = &judge.Standing{Route: standing.Route, Seconds: int(standing.Seconds)}
	}
	return out
}

// judgeError turns the agent's refusal into one with its reason kept.
func judgeError(response *http.Response) error {
	var failure sandboxapi.ErrorResponse
	_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure)
	said := strings.TrimSpace(failure.Error)
	if said == "" {
		said = fmt.Sprintf("the judge answered HTTP %d", response.StatusCode)
	}
	status := http.StatusBadGateway
	if response.StatusCode == http.StatusTooManyRequests {
		// Every judging run was in use until the deadline. That is not a
		// verdict either, but it is worth telling apart from a judge that
		// cannot answer at all.
		status = http.StatusTooManyRequests
	}
	return apperrors.NewStatusError(status, said)
}
