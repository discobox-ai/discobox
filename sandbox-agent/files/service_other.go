//go:build !linux

package files

import (
	"context"
	"io"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// Service does each operation as the discobox's default user. A sandbox off
// Linux has one account, which the agent already is (ADR 0145 §5), so it does
// the operation itself.
type Service struct{}

// New is the service.
func New() (*Service, error) { return &Service{}, nil }

// Do performs req. user is the one account the agent runs as, resolved to
// check that it is; there is nobody else to be.
func (s *Service) Do(_ context.Context, _ *execs.User, req Request, body io.Reader) (Result, io.ReadCloser, error) {
	return Do(req, body)
}
