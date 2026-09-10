package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/discobox-ai/discobox/judge"
)

var poolJudgeClient = &http.Client{Timeout: judge.Timeout + 5*time.Second, Transport: &http.Transport{
	DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", judge.SocketPath)
	},
}}

func callPoolJudge(ctx context.Context, job judge.Job) (judge.Verdict, error) {
	data, err := json.Marshal(job)
	if err != nil {
		return judge.Verdict{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pool-judge/judge", bytes.NewReader(data))
	if err != nil {
		return judge.Verdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := poolJudgeClient.Do(req)
	if err != nil {
		return judge.Verdict{}, fmt.Errorf("pool judge unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return judge.Verdict{}, fmt.Errorf("pool judge unavailable (HTTP %d)", resp.StatusCode)
	}
	var v judge.Verdict
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*(judge.MaxInput+judge.MaxOutput)+4096)).Decode(&v); err != nil {
		return judge.Verdict{}, err
	}
	if v.Role != judge.Role || v.PromptVersion == "" || v.Revision == "" || v.Reason == "" {
		return v, fmt.Errorf("incomplete pool judge verdict")
	}
	return v, nil
}
