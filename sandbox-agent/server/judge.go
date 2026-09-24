package server

import (
	"context"
	"net/http"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
)

// JudgeSandbox puts one judging job to this discobox's harness (ADR 0148).
//
// The job arrives as evidence and an approved use, and nothing else: the
// prompt, the schema, the model role and the tools restriction are the
// runtime's, so a caller cannot ask a kinder question than the one Discobox
// asks. What comes back is a decision or an ask to be shown the body, and
// anything that is neither is an error rather than an answer.
func (h *handler) JudgeSandbox(ctx context.Context, req *sandboxapi.JudgeJob, _ sandboxapi.JudgeSandboxParams) (*sandboxapi.JudgeAnswer, error) {
	answer, err := h.terminals.Judge(ctx, judgeJob(req))
	if err != nil {
		if terminal.Busy(err) {
			// Not a refusal: the caller holding a request decides whether to
			// wait for the judge or give up on it.
			return nil, errorStatus(http.StatusTooManyRequests, err.Error())
		}
		return nil, err
	}
	out := &sandboxapi.JudgeAnswer{Reason: answer.Reason}
	if answer.Need != nil {
		need := sandboxapi.JudgeNeed{Body: sandboxapi.JudgeNeedBody(answer.Need.Body)}
		// A judge that named no budget said nothing about bytes, and sending
		// a zero would say it asked for none.
		if answer.Need.Bytes > 0 {
			need.Bytes = sandboxapi.NewOptInt64(int64(answer.Need.Bytes))
		}
		out.Need = sandboxapi.NewOptJudgeNeed(need)
		return out, nil
	}
	out.Allow = sandboxapi.NewOptBool(answer.Allow)
	return out, nil
}

// judgeJob is the job as this process judges it. It is mapped field by field
// rather than re-decoded from JSON: the wire type and the contract are two
// declarations of one thing, and a silent mismatch between them would be a
// judge answering about evidence nobody sent.
func judgeJob(in *sandboxapi.JudgeJob) judge.Job {
	job := judge.Job{
		Kind:       string(in.Kind),
		Purpose:    in.Purpose,
		Host:       in.Host,
		Credential: in.Credential.Or(""),
		Round:      int(in.Round),
		Command:    in.Command,
	}
	evidence, ok := in.Request.Get()
	if !ok {
		return job
	}
	job.Request = &judge.Request{Method: evidence.Method, URL: evidence.URL}
	if headers, ok := evidence.Headers.Get(); ok && len(headers) > 0 {
		job.Request.Headers = map[string][]string(headers)
	}
	if body, ok := evidence.Body.Get(); ok {
		job.Request.Body = &judge.Body{
			MediaType: body.MediaType.Or(""),
			Length:    body.Length.Or(0),
			Form:      string(body.Form.Or("")),
			Content:   body.Content.Or(""),
			Missing:   body.Missing.Or(""),
		}
	}
	return job
}
