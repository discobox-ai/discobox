package files

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// Service does each operation as the discobox's default user. On Linux the
// agent is root, so the operation runs in a helper process started with the
// user's credential, the way an exec's process is: what the user may not
// touch, the kernel refuses, and no check here can race the path it checked
// (ADR 26-10-22-499 §2).
type Service struct {
	helper helper
}

// New is the service, with this binary as its helper.
func New() (*Service, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find the files helper: %w", err)
	}
	return &Service{helper: helper{argv: []string{exe, HelperCommand}}}, nil
}

// Do performs req as user, as execs.Manager.ResolveUser resolved the default
// one. A nil user is an exec that inherits the agent's identity, and so is the
// helper.
func (s *Service) Do(ctx context.Context, user *execs.User, req Request, body io.Reader) (Result, io.ReadCloser, error) {
	attr, err := execs.AgentSysProcAttr(user)
	if err != nil {
		return Result{}, nil, err
	}
	return s.helper.do(ctx, attr, req, body)
}
