package server

import (
	"context"
	"encoding/json"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/judge"
)

func (h *handler) JudgeSandbox(ctx context.Context, req *sandboxapi.JudgeJob, _ sandboxapi.JudgeSandboxParams) (*sandboxapi.JudgeVerdict, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var job judge.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	verdict, err := h.terminals.Judge(ctx, job)
	if err != nil {
		verdict.Allow = false
		if verdict.Reason == "" {
			verdict.Reason = "judge runtime unavailable"
		}
	}
	data, err = json.Marshal(verdict)
	if err != nil {
		return nil, err
	}
	var out sandboxapi.JudgeVerdict
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
