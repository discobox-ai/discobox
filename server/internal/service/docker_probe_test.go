package service

import (
	"context"
	"testing"

	"github.com/moby/moby/client"
)

// skipWithoutDocker skips tests that initialize sandbox providers, which
// connect to the local Docker daemon. They run everywhere a daemon is
// reachable (dev machines, docker-enabled CI) and skip in environments
// without one (e.g. containerized test runs).
func skipWithoutDocker(t *testing.T) {
	t.Helper()
	dockerClient, err := client.New(client.FromEnv)
	if err == nil {
		_, err = dockerClient.Ping(context.Background(), client.PingOptions{})
	}
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
}
