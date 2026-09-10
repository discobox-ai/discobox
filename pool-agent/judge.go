package poolagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/pool-agent/judges"
	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/pool-agent/wire"
)

func servePoolJudge(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration, runtime sandboxruntime.Runtime) error {
	base, client, err := wire.HTTPClient(bootstrap.ControlPlaneURL, 30*time.Second)
	if err != nil {
		return err
	}
	fetch := func(ctx context.Context) (*api.PoolJudgeRuntimeResponse, error) {
		token, err := poolauth.CreateToken(registration.PrivateKey, poolauth.Claims{ProjectID: bootstrap.ProjectID, PoolID: bootstrap.PoolID})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/pools/"+bootstrap.PoolID+"/judge-runtime", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("judge runtime configuration unavailable (HTTP %d)", resp.StatusCode)
		}
		var spec api.PoolJudgeRuntimeResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&spec); err != nil {
			return nil, err
		}
		return &spec, nil
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
