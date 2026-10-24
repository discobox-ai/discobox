package judges

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/pkg/wellknown"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
)

// requestFacts is what the judge is told about a request: the discobox asking
// (ADR 26-10-14-691), read from its row for every job — a use can be held by
// more than one discobox, and "this discobox" is the one asking now — and,
// when the request is a call to the discobox API that names a discobox in
// the project, that discobox too (ADR 26-10-14-320 §5). The call names its
// target only by an ID; what that discobox was created with, and by whom, is
// read from its row here, as the asking discobox's is.
func (s *Service) requestFacts(ctx context.Context, projectID string, asking *model.Sandbox, request *judge.Request) ([]string, error) {
	targetID, _, ok := namedDiscobox(projectID, request)
	if !ok {
		return judge.Facts(judge.DiscoboxOf(asking.PolicyFacts())), nil
	}
	target, err := s.store.GetSandbox(ctx, projectID, targetID)
	if errors.Is(err, store.ErrNotFound) {
		// A discobox that is not in the project is no target to state: the
		// sandbox role refuses the call when it reaches the control plane.
		return judge.Facts(judge.DiscoboxOf(asking.PolicyFacts())), nil
	}
	if err != nil {
		return nil, err
	}
	return judge.TargetFacts(judge.DiscoboxOf(asking.PolicyFacts()), judge.DiscoboxOf(target.PolicyFacts())), nil
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

// commandFacts is what the judge is told about a command: the discobox asking,
// read from its row as for a request, and, when the command spends the
// discobox API's credential, every discobox of the project it names by its ID,
// in the order it names them (ADR 26-10-14-320 §5); what each was created
// with, and by whom, is read from its row. An ID is a discobox's only
// identity, so nothing else names one here: a name or a short ID is a label
// the CLI resolves, which another discobox can carry or come to match, and
// the judge is told that a discobox a command names that way has no creator
// Discobox vouches for (judge.System).
func (s *Service) commandFacts(ctx context.Context, projectID string, asking *model.Sandbox, host string, command []string) ([]string, error) {
	gate, known := wellknown.Lookup(wellknown.DiscoboxSandbox)
	if !known || hostscope.Normalize(host) != hostscope.Normalize(gate.Host()) {
		return judge.Facts(judge.DiscoboxOf(asking.PolicyFacts())), nil
	}
	var named []judge.Discobox
	for _, targetID := range namedDiscoboxes(command) {
		target, err := s.store.GetSandbox(ctx, projectID, targetID)
		if errors.Is(err, store.ErrNotFound) {
			// A discobox that is not in the project is no target to state,
			// as for a request.
			continue
		}
		if err != nil {
			return nil, err
		}
		named = append(named, judge.DiscoboxOf(target.PolicyFacts()))
	}
	if len(named) == 0 {
		return judge.Facts(judge.DiscoboxOf(asking.PolicyFacts())), nil
	}
	return judge.CommandFacts(judge.DiscoboxOf(asking.PolicyFacts()), named...), nil
}

// namedDiscoboxes is every discobox ID a command's arguments name, once each,
// in the order they first name it: an argument that is one, and one inside an
// argument — a flag's value after "=", a segment of an API path — set apart by
// characters no ID has. An ID is whole, in either spelling the CLI reads:
// "sbx_…", and "sbx-…" as a discobox's hostname spells it (id.Canonical).
func namedDiscoboxes(command []string) []string {
	var named []string
	for _, argument := range command {
		for _, word := range strings.FieldsFunc(argument, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-'
		}) {
			full := id.Canonical(id.PrefixSandbox, word)
			if strings.HasPrefix(full, id.PrefixSandbox+"_") && id.IsGenerated(full) && !slices.Contains(named, full) {
				named = append(named, full)
			}
		}
	}
	return named
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
	return judge.DelegationFacts(judge.DiscoboxOf(giving.PolicyFacts()), judge.DiscoboxOf(ask.For.PolicyFacts())), nil
}

// asking is the record of the discobox asking. The pool names the discobox,
// and the caller has already checked that against the use; what is said about
// it, and what a policy's variables bind to, is read here.
func (s *Service) asking(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "the discobox asking was not found")
	}
	return sandbox, nil
}

// underPolicy is job as it is judged under use: against the grant's policy,
// bound to the discobox asking, when the grant carries one (ADR 26-10-20-782
// §1), and otherwise against the use's sentence, as it was built. A policy
// with no allow left for this discobox is refused here, since a judge could
// only refuse it too.
func underPolicy(job judge.Job, use services.ApprovedUse, asking *model.Sandbox) (judge.Job, error) {
	if use.Policy == nil {
		return job, nil
	}
	job, err := job.UnderPolicy(*use.Policy, asking.PolicyFacts())
	if errors.Is(err, judge.ErrPolicyAllowsNothing) {
		return judge.Job{}, apperrors.NewStatusError(http.StatusForbidden, err.Error())
	}
	return job, err
}
