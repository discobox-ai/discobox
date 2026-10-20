package judges

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/policy"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
)

// facts is what the judge is told about the discobox asking (ADR
// 26-10-14-691), read from its row for every job: a use can be held by more
// than one discobox, and "this discobox" is the one asking now. The pool names
// the discobox, and the caller has already checked that against the use; what
// is said about it is read here.
func (s *Service) facts(ctx context.Context, projectID, sandboxID string) ([]string, error) {
	asking, err := s.asking(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	return judge.Facts(asking), nil
}

// requestFacts is what the judge is told about a request: the discobox asking,
// and, when the request is a call to the discobox API that names a discobox in
// the project, that discobox too (ADR 26-10-14-320 §5). The call names its
// target only by an ID; what that discobox was created with, and by whom, is
// read from its row here, as the asking discobox's is.
func (s *Service) requestFacts(ctx context.Context, projectID, sandboxID string, request *judge.Request) ([]string, error) {
	asking, err := s.asking(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	targetID, _, ok := namedDiscobox(projectID, request)
	if !ok {
		return judge.Facts(asking), nil
	}
	target, err := s.store.GetSandbox(ctx, projectID, targetID)
	if errors.Is(err, store.ErrNotFound) {
		// A discobox that is not in the project is no target to state: the
		// sandbox role refuses the call when it reaches the control plane.
		return judge.Facts(asking), nil
	}
	if err != nil {
		return nil, err
	}
	return judge.TargetFacts(asking, target.JudgeDiscobox()), nil
}

// namedDiscobox is the discobox a request to the discobox API names in its
// path — /projects/{projectId}/sandboxes/{sandboxId}/..., or the same under
// /api — when the project is this one, or "default", which for a sandbox's
// call is its own. segment is where in the path it is named, counted from the
// path's first segment.
func namedDiscobox(projectID string, request *judge.Request) (id string, segment int, ok bool) {
	gate, known := wellknown.Lookup(wellknown.DiscoboxSandbox)
	if !known || request == nil {
		return "", 0, false
	}
	target, err := url.Parse(request.URL)
	if err != nil || hostscope.Normalize(target.Host) != hostscope.Normalize(gate.Host()) {
		return "", 0, false
	}
	segments := strings.Split(strings.TrimPrefix(target.Path, "/"), "/")
	if len(segments) > 0 && segments[0] == "api" {
		segment = 1
	}
	rest := segments[segment:]
	if len(rest) < 4 || rest[0] != "projects" || rest[2] != "sandboxes" || rest[3] == "" {
		return "", 0, false
	}
	if rest[1] != projectID && rest[1] != "default" {
		return "", 0, false
	}
	return rest[3], segment + 3, true
}

// delegationFacts is what the judge is told about a delegation: the discobox
// handing the credential on, read from its row, and the one it would hand it
// to, from the row it was given — on a create that one is not stored yet.
func (s *Service) delegationFacts(ctx context.Context, projectID string, ask services.DelegationAsk) ([]string, error) {
	if ask.For == nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a delegation is judged knowing the discobox it hands the credential to")
	}
	giving, err := s.asking(ctx, projectID, ask.ApproverID)
	if err != nil {
		return nil, err
	}
	return judge.DelegationFacts(giving, ask.For.JudgeDiscobox()), nil
}

// variables is what a policy's variables bind to for the discobox asking, now
// (ADR 26-10-20-782 §4): each that resolved when the policy was approved and
// resolves to the same value from the discobox's record today. One that does
// not is left out, and matches nothing.
func (s *Service) variables(ctx context.Context, projectID, sandboxID string, p policy.Policy) (map[string]string, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "the discobox asking was not found")
	}
	var bound map[string]string
	for variable, binding := range p.Bind(sandbox.PolicyFacts()) {
		if value, ok := binding.Value(); ok {
			if bound == nil {
				bound = map[string]string{}
			}
			bound[variable] = value
		}
	}
	return bound, nil
}

// asking is the record of the discobox asking.
func (s *Service) asking(ctx context.Context, projectID, sandboxID string) (judge.Discobox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return judge.Discobox{}, apperrors.NotFound(err, "the discobox asking was not found")
	}
	return sandbox.JudgeDiscobox(), nil
}
