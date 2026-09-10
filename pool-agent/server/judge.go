package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/discobox-ai/discobox/judge"
	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
)

// NewJudgeHandler is served only over the pool-private, root-owned Unix socket.
// The public router constructs no judge service and cannot invoke this path.
func NewJudgeHandler(run func(context.Context, judge.Job) (judge.Verdict, error)) (http.Handler, error) {
	handler := &sandboxService{judge: run}
	server, err := workerapi.NewServer(handler, handler)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("POST /judge", server)
	return mux, nil
}

func (s *sandboxService) PoolJudge(ctx context.Context, req *workerapi.JudgeJob) (*workerapi.JudgeVerdict, error) {
	if s.judge == nil {
		return nil, newStatusError(http.StatusForbidden, "pool-private judge endpoint")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var job judge.Job
	if err = json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	verdict, err := s.judge(ctx, job)
	if err != nil {
		verdict.Allow = false
		if verdict.Reason == "" {
			verdict.Reason = "pool judge unavailable"
		}
	}
	data, err = json.Marshal(verdict)
	if err != nil {
		return nil, err
	}
	var out workerapi.JudgeVerdict
	if err = json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
