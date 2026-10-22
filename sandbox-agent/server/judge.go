package server

import (
	"context"
	"encoding/json"
	"net/http"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
)

// JudgeSandbox puts one judging job to this discobox's harness (ADR 26-09-22-838).
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
		need := sandboxapi.JudgeNeed{Body: answer.Need.Body}
		// A judge that named no budget said nothing about bytes, and sending
		// a zero would say it asked for none.
		if answer.Need.Bytes > 0 {
			need.Bytes = sandboxapi.NewOptInt64(int64(answer.Need.Bytes))
		}
		out.Need = sandboxapi.NewOptJudgeNeed(need)
		return out, nil
	}
	out.Allow = sandboxapi.NewOptBool(answer.Allow)
	if answer.Standing != nil {
		// Passed on as the judge asked for it. Whether it stands, and for how
		// long, is the control plane's to decide (ADR 26-09-25-428 §2).
		out.Standing = sandboxapi.NewOptJudgeStanding(sandboxapi.JudgeStanding{
			Route:   answer.Standing.Route,
			Seconds: int64(answer.Standing.Seconds),
		})
	}
	return out, nil
}

// judgeJob is the job as this process judges it. It is mapped field by field
// rather than re-decoded from JSON: the wire type and the contract are two
// declarations of one thing, and a silent mismatch between them would be a
// judge answering about evidence nobody sent.
func judgeJob(in *sandboxapi.JudgeJob) judge.Job {
	job := judge.Job{
		Kind:       string(in.Kind),
		Purpose:    in.Purpose.Or(""),
		Purposes:   in.Purposes,
		Host:       in.Host,
		Credential: in.Credential.Or(""),
		Round:      int(in.Round),
		Command:    in.Command,
		Guidance:   in.Guidance,
		Facts:      in.Facts,
		Uses:       in.Uses,
	}
	if definitions, ok := in.Definitions.Get(); ok && len(definitions) > 0 {
		job.Definitions = map[string]string(definitions)
	}
	if stdin, ok := in.Stdin.Get(); ok {
		job.Stdin = &judge.Input{Content: stdin.Content, Missing: stdin.Missing.Or("")}
	}
	if reported, ok := in.Reported.Get(); ok {
		job.Reported = &judge.Reported{
			WorkingDirectory: reported.WorkingDirectory.Or(""),
			RepositoryRoot:   reported.RepositoryRoot.Or(""),
			RefCommit:        reported.RefCommit.Or(""),
			RefSubject:       reported.RefSubject.Or(""),
		}
	}
	evidence, ok := in.Request.Get()
	if !ok {
		return job
	}
	job.Request = &judge.Request{Method: evidence.Method, URL: evidence.URL}
	if headers, ok := evidence.Headers.Get(); ok && len(headers) > 0 {
		job.Request.Headers = map[string][]string(headers)
	}
	job.Request.Protocol = recognition(evidence.Protocol)
	job.Request.Endpoint = recognition(evidence.Endpoint)
	if body, ok := evidence.Body.Get(); ok {
		job.Request.Body = &judge.Body{
			MediaType:  body.MediaType.Or(""),
			Length:     body.Length.Or(0),
			Parser:     recognition(body.Parser),
			Metadata:   metadata(body.Metadata),
			ParseError: body.ParseError.Or(""),
			Missing:    body.Missing.Or(""),
		}
		if content, ok := body.Content.Get(); ok {
			job.Request.Body.Content = &content
		}
	}
	return job
}

// ProposeSandboxRule has this discobox's harness write the policy a person
// starts an approval rule from (ADR 26-10-12-330 §7). Like a
// judging job, the caller supplies data and nothing else: the prompt, the
// schema, the model role and the tools restriction are the runtime's.
func (h *handler) ProposeSandboxRule(ctx context.Context, req *sandboxapi.JudgeProposalJob, _ sandboxapi.ProposeSandboxRuleParams) (*sandboxapi.JudgeProposal, error) {
	proposal, err := h.terminals.Propose(ctx, judge.ProposalJob{
		Statements:  req.Statements,
		Definitions: map[string]string(req.Definitions.Or(nil)),
		Credential:  req.Credential.Or(""),
		Hosts:       req.Hosts,
	})
	if err != nil {
		if terminal.Busy(err) {
			return nil, errorStatus(http.StatusTooManyRequests, err.Error())
		}
		return nil, err
	}
	return &sandboxapi.JudgeProposal{
		StatementsAnyTarget: proposal.StatementsAnyTarget,
	}, nil
}

// recognition is what the request was recognized as, when it was.
func recognition(in sandboxapi.OptJudgeRecognition) *judge.Recognition {
	recognized, ok := in.Get()
	if !ok {
		return nil
	}
	return &judge.Recognition{Name: recognized.Name, Version: int(recognized.Version)}
}

// metadata is a parser's metadata as the contract holds it, one JSON object.
// A value that would not write back is left out with the rest of it, and the
// job is judged without it, which only ever means the judge is shown less.
func metadata(in sandboxapi.OptJudgeRequestBodyMetadata) json.RawMessage {
	fields, ok := in.Get()
	if !ok || len(fields) == 0 {
		return nil
	}
	object := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		object[key] = json.RawMessage(value)
	}
	data, err := json.Marshal(object)
	if err != nil {
		return nil
	}
	return data
}
