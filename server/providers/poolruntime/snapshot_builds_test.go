package poolruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// buildRuntime is a pool with one build under way, keyed "k1", whose logs
// name what was asked and whose exec echoes its input.
type buildRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
	ended context.Context
}

func (r *buildRuntime) SnapshotBuildLogs(_ context.Context, key string, req sandboxruntime.SnapshotLogsRequest, w io.Writer) error {
	if key != "k1" {
		return sandboxruntime.ErrSnapshotBuildNotFound
	}
	_, err := fmt.Fprintf(w, "%s %d %v\n", req.Feature, req.Lines, req.Follow)
	return err
}

func (r *buildRuntime) SnapshotBuildExec(_ context.Context, key string, _ machine.ExecRequest) (machine.Process, context.Context, error) {
	if key != "k1" {
		return nil, nil, sandboxruntime.ErrSnapshotBuildQueued
	}
	reader, writer := io.Pipe()
	return &pipeProcess{in: writer, out: reader}, r.ended, nil
}

type pipeProcess struct {
	in  *io.PipeWriter
	out *io.PipeReader
}

func (p *pipeProcess) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *pipeProcess) CloseStdin() error           { return p.in.Close() }
func (p *pipeProcess) Resize(uint16, uint16) error { return nil }
func (p *pipeProcess) Close() error                { _ = p.in.Close(); return p.out.Close() }
func (p *pipeProcess) Wait(stdout, _ io.Writer) (int, error) {
	_, err := io.Copy(stdout, p.out)
	return 0, err
}

func newSnapshotBuildProvider(t *testing.T) *Provider {
	t.Helper()
	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, "project-1", "pool-1", poolagentserver.ScopeSnapshotBuildRead, poolagentserver.ScopeSnapshotBuildExec)
	router, err := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               &buildRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), ended: context.Background()},
		ControlPlanePublicKey: controlPlaneKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(router)
	t.Cleanup(agent.Close)
	runtimeProvider := &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: poolToken}
	return New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, &fakePoolManager{pool: activePool("pool-1"), schedulable: true}, nil)
}

// A build's logs are read through its pool agent, and a key that names no
// build is not found.
func TestPoolProviderReadsASnapshotBuildsLogsThroughTheAgent(t *testing.T) {
	provider := newSnapshotBuildProvider(t)
	ctx := context.Background()
	logs, err := provider.OpenSnapshotBuildLogs(ctx, activePool("pool-1"), "k1", sandbox.SnapshotBuildLogOptions{Feature: "docker", Tail: 7, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(logs)
	_ = logs.Close()
	if err != nil || string(body) != "docker 7 true\n" {
		t.Fatalf("logs = %q, %v", body, err)
	}
	if _, err := provider.OpenSnapshotBuildLogs(ctx, activePool("pool-1"), "k2", sandbox.SnapshotBuildLogOptions{}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("logs of no build = %v, want not found", err)
	}
}

// An exec into a build is a connection of execstream frames to its pool
// agent's exec.
func TestPoolProviderOpensAnExecInASnapshotBuildThroughTheAgent(t *testing.T) {
	provider := newSnapshotBuildProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := provider.DialSnapshotBuildExec(ctx, activePool("pool-1"), "k1", sandbox.ConsoleOptions{Command: []string{"cat"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := frame.Write(conn, frame.Input, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	next, err := frame.Read(conn)
	if err != nil || next.Type != frame.Stdout || string(next.Payload) != "hi" {
		t.Fatalf("frame %d %q, %v", next.Type, next.Payload, err)
	}
}

// An agent too old for the routes answers the router's text/plain 404, which
// is the pool being unable to reach its builds, not a missing build.
func TestPoolProviderSeparatesAMissingSnapshotBuildRouteFromAMissingBuild(t *testing.T) {
	agent := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(agent.Close)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.baseURL = agent.URL
	runtimeProvider.client = agent.Client()
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, &fakePoolManager{pool: activePool("pool-1"), schedulable: true}, nil)
	if _, err := provider.OpenSnapshotBuildLogs(context.Background(), activePool("pool-1"), "k1", sandbox.SnapshotBuildLogOptions{}); !errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		t.Fatalf("logs from an old agent = %v, want unsupported", err)
	}
}
