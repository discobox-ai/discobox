package handlers

import (
	"context"
	"net/http"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// JudgeForPool puts a job to the judge of the project that owns the asking
// pool (ADR 0150 §2). The pool is authenticated as itself; which judge answers
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
		need := serverapi.JudgeNeed{Body: serverapi.JudgeNeedBody(answer.Need.Body)}
		if answer.Need.Bytes > 0 {
			need.Bytes = serverapi.NewOptInt64(int64(answer.Need.Bytes))
		}
		out.Need = serverapi.NewOptJudgeNeed(need)
		return out, nil
	}
	out.Allow = serverapi.NewOptBool(answer.Allow)
	return out, nil
}

// judgeAskFrom is the ask as this server reads it. It is mapped field by field
// rather than re-decoded from JSON: the wire type and the contract are two
// declarations of one thing, and a silent mismatch between them would be a
// judge answering about evidence nobody sent.
//
// Nothing here says what the use approves. The ask carries the discobox, the
// use and the evidence; the sentence being judged against is read from the
// live grant by the service (ADR 0150 §4).
func judgeAskFrom(in *serverapi.PoolJudgeAsk) services.JudgeAsk {
	ask := services.JudgeAsk{
		SandboxID: in.SandboxId,
		UseID:     in.UseId,
		Round:     int(in.Round),
		Command:   in.Command,
	}
	evidence := in.Request
	ask.Request = &judge.Request{Method: evidence.Method, URL: evidence.URL}
	if headers, ok := evidence.Headers.Get(); ok && len(headers) > 0 {
		ask.Request.Headers = map[string][]string(headers)
	}
	if body, ok := evidence.Body.Get(); ok {
		ask.Request.Body = &judge.Body{
			MediaType: body.MediaType.Or(""),
			Length:    body.Length.Or(0),
			Form:      string(body.Form.Or("")),
			Content:   body.Content.Or(""),
			Missing:   body.Missing.Or(""),
		}
	}
	return ask
}
