package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// JudgeForPool puts a job to the judge of the project that owns the asking
// pool (ADR 26-09-22-838 §2). The pool is authenticated as itself; which judge answers
// is the control plane's to decide, not the pool's to name.
func (h *Handler) JudgeForPool(ctx context.Context, req *serverapi.PoolJudgeAsk, params serverapi.JudgeForPoolParams) (serverapi.JudgeForPoolRes, error) {
	principal, err := credentialBrokerPrincipal(ctx)
	if err != nil {
		return apiError(err), nil
	}
	if principal.PoolID != params.PoolId {
		return apiError(apperrors.NewStatusError(http.StatusForbidden, "pool assertion does not match the pool in the route")), nil
	}
	answer, err := h.services.Judges.Judge(ctx, principal.PoolID, judgeAskFrom(req))
	if err != nil {
		return apiError(err), nil
	}
	out := &serverapi.JudgeAnswer{Reason: answer.Reason}
	if answer.Need != nil {
		need := serverapi.JudgeNeed{Body: answer.Need.Body}
		if answer.Need.Bytes > 0 {
			need.Bytes = serverapi.NewOptInt64(int64(answer.Need.Bytes))
		}
		out.Need = serverapi.NewOptJudgeNeed(need)
		return out, nil
	}
	out.Allow = serverapi.NewOptBool(answer.Allow)
	return out, nil
}

// JudgeCommandForPool asks the judge of the project that owns the asking pool
// about a command one of its discoboxes is about to run, before the pool
// mints anything for it (ADR 26-09-22-838 §3). Like a request, the pool names
// the discobox and the use, and what the use approves is read here.
func (h *Handler) JudgeCommandForPool(ctx context.Context, req *serverapi.PoolCommandAsk, params serverapi.JudgeCommandForPoolParams) (serverapi.JudgeCommandForPoolRes, error) {
	principal, err := credentialBrokerPrincipal(ctx)
	if err != nil {
		return apiError(err), nil
	}
	if principal.PoolID != params.PoolId {
		return apiError(apperrors.NewStatusError(http.StatusForbidden, "pool assertion does not match the pool in the route")), nil
	}
	answer, err := h.services.Judges.JudgeCommand(ctx, principal.PoolID, commandAskFrom(req))
	if err != nil {
		return apiError(err), nil
	}
	return &serverapi.JudgeAnswer{Allow: serverapi.NewOptBool(answer.Allow), Reason: answer.Reason}, nil
}

// commandAskFrom is the command ask as this server reads it, mapped field by
// field for the reason judgeAskFrom is.
func commandAskFrom(in *serverapi.PoolCommandAsk) services.CommandAsk {
	ask := services.CommandAsk{SandboxID: in.SandboxId, UseID: in.UseId, Command: in.Command}
	if stdin, ok := in.Stdin.Get(); ok {
		ask.Stdin = &judge.Input{Content: stdin.Content, Missing: stdin.Missing.Or("")}
	}
	if reported, ok := in.Reported.Get(); ok {
		ask.Reported = &judge.Reported{
			WorkingDirectory: reported.WorkingDirectory.Or(""),
			RepositoryRoot:   reported.RepositoryRoot.Or(""),
			RefCommit:        reported.RefCommit.Or(""),
			RefSubject:       reported.RefSubject.Or(""),
		}
	}
	return ask
}

// judgeAskFrom is the ask as this server reads it. It is mapped field by field
// rather than re-decoded from JSON: the wire type and the contract are two
// declarations of one thing, and a silent mismatch between them would be a
// judge answering about evidence nobody sent.
//
// Nothing here says what the use approves. The ask carries the discobox, the
// use and the evidence; the sentence being judged against is read from the
// live grant by the service (ADR 26-09-22-838 §4).
func judgeAskFrom(in *serverapi.PoolJudgeAsk) services.JudgeAsk {
	ask := services.JudgeAsk{
		SandboxID: in.SandboxId,
		UseID:     in.UseId,
		Round:     int(in.Round),
		Command:   in.Command,
	}
	if millis, ok := in.TimeoutMillis.Get(); ok && millis > 0 {
		ask.Timeout = time.Duration(millis) * time.Millisecond
	}
	evidence := in.Request
	ask.Request = &judge.Request{Method: evidence.Method, URL: evidence.URL}
	if headers, ok := evidence.Headers.Get(); ok && len(headers) > 0 {
		ask.Request.Headers = map[string][]string(headers)
	}
	ask.Request.Protocol = recognitionFrom(evidence.Protocol)
	ask.Request.Endpoint = recognitionFrom(evidence.Endpoint)
	if body, ok := evidence.Body.Get(); ok {
		ask.Request.Body = &judge.Body{
			MediaType:  body.MediaType.Or(""),
			Length:     body.Length.Or(0),
			Parser:     recognitionFrom(body.Parser),
			Metadata:   metadataFrom(body.Metadata),
			ParseError: body.ParseError.Or(""),
			Missing:    body.Missing.Or(""),
		}
		if content, ok := body.Content.Get(); ok {
			ask.Request.Body.Content = &content
		}
	}
	return ask
}

// recognitionFrom is what a pool says it recognized, when it says so. Whether
// the name is one worth anything is judge.Job.Validate's to say.
func recognitionFrom(in serverapi.OptJudgeRecognition) *judge.Recognition {
	recognized, ok := in.Get()
	if !ok {
		return nil
	}
	return &judge.Recognition{Name: recognized.Name, Version: int(recognized.Version)}
}

// metadataFrom is a parser's metadata as the contract holds it: one JSON
// object. The wire type is a map of raw values, which says the same thing
// with the keys in no particular order.
func metadataFrom(in serverapi.OptJudgeRequestBodyMetadata) json.RawMessage {
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
		// A value that is not JSON. Left out rather than passed on half
		// written: Validate would refuse it anyway, and a pool's metadata is
		// evidence, which is judged without it rather than not at all.
		return nil
	}
	return data
}
