package judges

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/judge"
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
	AcquireSandboxHTTPClientForServer(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error)
}

// SetLeases installs it. A judge that cannot be reached is a judge that
// refuses, so this is required before Judge answers anything.
func (s *Service) SetLeases(leases Leases) { s.leases = leases }

// Uses is the secrets service's half of saying what an approved use allows.
// It is named here for the same reason Leases is: this package makes one call,
// twice.
type Uses interface {
	ApprovedUse(ctx context.Context, poolID, sandboxID, useID, host string) (services.ApprovedUse, error)
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
	if !s.enabled {
		// Said in a way a program can recognize, because a pool has to tell it
		// apart from a judge that failed: one means stop asking, the other
		// means no credential goes out (ADR 26-09-22-838 §4).
		return judge.Answer{}, apperrors.NewStatusErrorOfKind(http.StatusServiceUnavailable,
			apperrors.KindJudgingDisabled, "this server does not judge credential use")
	}
	if s.leases == nil || s.uses == nil {
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
	judgeSandbox, err := s.judge(ctx, project)
	if err != nil {
		return judge.Answer{}, err
	}
	if judgeSandbox == nil {
		// Read-only: this is a pool asking, not the convergence deciding.
		_, why, err := s.wanted(ctx, project, false)
		if err != nil {
			return judge.Answer{}, err
		}
		if why == "" {
			why = "the project's judge is not ready yet"
		}
		return judge.Answer{}, apperrors.NewStatusError(http.StatusServiceUnavailable, "this project has no judge: "+why)
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
		return judge.Answer{}, apperrors.NewStatusError(http.StatusServiceUnavailable,
			"this project's judge could not be brought up; the server's log says why")
	}

	// The question is composed once there is something that could answer it:
	// reading a use out of the grants is work, and a project with no judge
	// refuses whatever the use turns out to say.
	job, use, err := s.job(ctx, poolID, ask)
	if err != nil {
		return judge.Answer{}, err
	}

	// One ask is bounded here as well as at the judge (ADR 26-09-22-838 §2): a caller
	// that passed no deadline must not be able to hold this goroutine, the
	// lease, and the judge's only slot for as long as the judge is willing to
	// think. Whichever deadline is sooner wins.
	ctx, cancel := context.WithTimeout(ctx, judge.Timeout+judgeRoutingGrace)
	defer cancel()

	// The round trip starts here, before the judge is reached: bringing up a
	// stopped judge is part of how long it took to answer.
	start := time.Now()
	lease, sandboxModel, err := s.leases.AcquireSandboxHTTPClientForServer(ctx, project.ID, judgeSandbox.ID, []string{poolagentauth.ScopeJudgeRun})
	if err != nil {
		return judge.Answer{}, err
	}
	defer lease.Release()

	target, err := sandboxagentclient.TargetURL(lease.BaseURL, sandboxModel.ProjectID, sandboxModel.PoolID, sandboxModel.ID, "/judge")
	if err != nil {
		return judge.Answer{}, err
	}
	// Marshaled through the pointer, which is what reaches the generated
	// MarshalJSON. By value, encoding/json walks the struct itself and asks
	// each unset optional field to marshal — and an unset one writes nothing,
	// which fails the whole encode. The generated encoder is the only one that
	// knows to leave an unset field out.
	jobBody := judgeJobBody(job)
	body, err := json.Marshal(&jobBody)
	if err != nil {
		return judge.Answer{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return judge.Answer{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := sandboxagentclient.HTTPClient(lease).Do(request)
	if err != nil {
		return judge.Answer{}, apperrors.NewStatusError(http.StatusBadGateway,
			fmt.Sprintf("the project's judge could not be reached: %v", err))
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return judge.Answer{}, judgeError(response)
	}
	var answered sandboxapi.JudgeAnswer
	if err := json.NewDecoder(io.LimitReader(response.Body, int64(judge.MaxOutput))).Decode(&answered); err != nil {
		return judge.Answer{}, apperrors.NewStatusError(http.StatusBadGateway,
			fmt.Sprintf("the project's judge answered with something unreadable: %v", err))
	}
	latency := time.Since(start)
	decided := answer(answered)
	// Asked again after the verdict, because a verdict takes a while and a
	// grant can be revoked inside it (ADR 26-09-22-838 §4). The check is the same one
	// the question was built from, so what it rules out is a use that stopped
	// being approved while a model was reading the request it authorized.
	//
	// Before the answer is recorded, not after: an answer about a use that no
	// longer exists is not what the request was refused on, and a row saying
	// allow for it would contradict the proxy's own record of the refusal.
	// Like an ask that got no answer, it leaves the proxy's blocked row alone.
	if _, err := s.uses.ApprovedUse(ctx, poolID, ask.SandboxID, ask.UseID, judgedHost(ask)); err != nil {
		return judge.Answer{}, err
	}
	// Recorded before the answer goes back, and gating it: an answer with no
	// record of it is no verdict (ADR 26-09-22-838 §§4, 8). The write gets a
	// deadline of its own, so an answer that arrived just inside the ask's is
	// not lost to it — the model has already been paid to give it.
	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancelRecord()
	if err := s.record(recordCtx, project.ID, judgeSandbox, ask, use, job, decided, latency); err != nil {
		return judge.Answer{}, err
	}
	return decided, nil
}

// recordTimeout bounds writing one verdict once the judge has answered.
const recordTimeout = 10 * time.Second

// record persists one answer as a request verdict. The row is written whatever
// the answer — an allow, a refusal, or an ask to be shown the body — because
// each is the judge's decision about a request that was held while it was
// made. The judge is read off the discobox that answered, so a verdict names
// the harness and the image that produced it even after the project's judge
// is replaced.
func (s *Service) record(ctx context.Context, projectID string, judgeSandbox *model.Sandbox, ask services.JudgeAsk, use services.ApprovedUse, job judge.Job, decided judge.Answer, latency time.Duration) error {
	prompt, err := judge.Prompt(job)
	if err != nil {
		return err
	}
	row := &model.CredentialVerdict{
		ProjectID:      projectID,
		Kind:           model.CredentialVerdictKindRequest,
		Origin:         model.CredentialVerdictOriginJudge,
		SandboxID:      ask.SandboxID,
		UseID:          ask.UseID,
		GrantID:        use.GrantID,
		Command:        job.Command,
		Request:        job.Request,
		Round:          job.Round,
		Allow:          decided.Allow,
		Need:           decided.Need,
		Reason:         decided.Reason,
		Role:           judge.Role,
		Prompt:         prompt,
		PromptVersion:  judge.PromptVersion,
		LatencyMS:      latency.Milliseconds(),
		JudgeSandboxID: judgeSandbox.ID,
		Image:          judgeSandbox.Image,
		ImageDigest:    judgeSandbox.ImageDigest,
	}
	if judgeSandbox.HarnessConfigID != nil {
		row.HarnessConfigID = *judgeSandbox.HarnessConfigID
	}
	return s.store.CreateCredentialVerdict(ctx, row)
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
	use, err := s.uses.ApprovedUse(ctx, poolID, ask.SandboxID, ask.UseID, judgedHost(ask))
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
	}
	if err := job.Validate(); err != nil {
		return judge.Job{}, services.ApprovedUse{}, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	return job, use, nil
}

// judgedHost is where the request the pool observed is going, which is what
// the use has to cover. It is read off the URL rather than taken as a field of
// its own: the destination and the evidence must be the same destination.
func judgedHost(ask services.JudgeAsk) string {
	if ask.Request == nil {
		return ""
	}
	target, err := url.Parse(ask.Request.URL)
	if err != nil || target.Host == "" {
		return ""
	}
	return hostscope.Normalize(target.Host)
}

// judgeRoutingGrace is what this hop allows on top of the judge's own bound:
// the time to reach the pool, start a stopped judge, and read the answer back.
// It is generous on purpose — the deadline here is a backstop against a caller
// with none, not the one that should normally fire.
const judgeRoutingGrace = 30 * time.Second

// judgeJobBody is the job on the wire to the judge's agent.
func judgeJobBody(job judge.Job) sandboxapi.JudgeJob {
	body := sandboxapi.JudgeJob{
		Kind:    sandboxapi.JudgeJobKind(job.Kind),
		Purpose: job.Purpose,
		Host:    job.Host,
		Round:   int64(job.Round),
		Command: job.Command,
	}
	if job.Credential != "" {
		body.Credential = sandboxapi.NewOptString(job.Credential)
	}
	if job.Request == nil {
		return body
	}
	evidence := sandboxapi.JudgeRequestEvidence{Method: job.Request.Method, URL: job.Request.URL}
	if len(job.Request.Headers) > 0 {
		evidence.Headers = sandboxapi.NewOptJudgeRequestEvidenceHeaders(job.Request.Headers)
	}
	if job.Request.Body != nil {
		requestBody := sandboxapi.JudgeRequestBody{Length: sandboxapi.NewOptInt64(job.Request.Body.Length)}
		if job.Request.Body.MediaType != "" {
			requestBody.MediaType = sandboxapi.NewOptString(job.Request.Body.MediaType)
		}
		if job.Request.Body.Form != "" {
			requestBody.Form = sandboxapi.NewOptJudgeRequestBodyForm(sandboxapi.JudgeRequestBodyForm(job.Request.Body.Form))
		}
		if job.Request.Body.Content != "" {
			requestBody.Content = sandboxapi.NewOptString(job.Request.Body.Content)
		}
		if job.Request.Body.Missing != "" {
			requestBody.Missing = sandboxapi.NewOptString(job.Request.Body.Missing)
		}
		evidence.Body = sandboxapi.NewOptJudgeRequestBody(requestBody)
	}
	body.Request = sandboxapi.NewOptJudgeRequestEvidence(evidence)
	return body
}

// answer is what the judge said, in the words this server passes on.
func answer(answered sandboxapi.JudgeAnswer) judge.Answer {
	out := judge.Answer{Reason: answered.Reason, Allow: answered.Allow.Or(false)}
	if need, ok := answered.Need.Get(); ok {
		out.Allow = false
		out.Need = &judge.Need{Body: string(need.Body), Bytes: int(need.Bytes.Or(0))}
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
