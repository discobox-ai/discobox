package poolagent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/pool-agent/judges"
	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolserver "github.com/discobox-ai/discobox/pool-agent/server"
)

// getJudgeRuntime uses the already-resolved control-plane transport. Its
// logical HTTP hostname is not a network address on Unix/VSOCK pools.
func (c *HTTPClient) getJudgeRuntime(ctx context.Context, projectID, poolID string, key ed25519.PrivateKey) (*api.PoolJudgeRuntimeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := poolauth.CreateToken(key, poolauth.Claims{ProjectID: projectID, PoolID: poolID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/pools/"+url.PathEscape(poolID)+"/judge-runtime", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&failure)
		return nil, fmt.Errorf("judge runtime configuration unavailable (HTTP %d): %s", resp.StatusCode, failure.Detail)
	}
	var spec api.PoolJudgeRuntimeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func servePoolJudge(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration, runtime sandboxruntime.Runtime, client *HTTPClient) error {
	fetch := func(ctx context.Context) (*api.PoolJudgeRuntimeResponse, error) {
		return client.getJudgeRuntime(ctx, bootstrap.ProjectID, bootstrap.PoolID, registration.PrivateKey)
	}
	service := judges.New(bootstrap.ProjectID, runtime, fetch)
	handler, err := poolserver.NewJudgeHandler(service.Judge)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(judge.SocketPath), 0o700); err != nil {
		return err
	}
	if err := os.Remove(judge.SocketPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "unix", judge.SocketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(judge.SocketPath, 0o600); err != nil {
		return err
	}
	server := &http.Server{Handler: http.MaxBytesHandler(handler, judge.MaxInput), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: judge.Timeout + 10*time.Second, WriteTimeout: judge.Timeout + 10*time.Second}
	go service.Run(ctx, logger)
	go func() { <-ctx.Done(); _ = server.Close() }()
	return server.Serve(listener)
}
