package judges

import (
	"context"
	"net/http"
	"sort"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
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
	return judge.DelegationFacts(giving, discobox(ask.For)), nil
}

// asking is the record of the discobox asking.
func (s *Service) asking(ctx context.Context, projectID, sandboxID string) (judge.Discobox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return judge.Discobox{}, apperrors.NotFound(err, "the discobox asking was not found")
	}
	return discobox(sandbox), nil
}

// discobox is the part of a sandbox's row the judge is told about: what was
// recorded when it was created, and nothing it reports about itself since.
func discobox(sandbox *model.Sandbox) judge.Discobox {
	out := judge.Discobox{SandboxID: sandbox.ID}
	if sandbox.CreatedBySandboxID != nil {
		out.CreatedBy = *sandbox.CreatedBySandboxID
	}
	if sandbox.Source != nil {
		primary := factSource(sandbox.Source)
		out.Primary = &primary
	}
	// By destination, so the facts a job is told are the same every time.
	destinations := make([]string, 0, len(sandbox.SourceCodeReferences))
	for destination := range sandbox.SourceCodeReferences {
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	for _, destination := range destinations {
		source := sandbox.SourceCodeReferences[destination]
		out.Additional = append(out.Additional, factSource(&source))
	}
	return out
}

// factSource is one source as it was created.
func factSource(source *model.GitSource) judge.Source {
	out := judge.Source{Remote: source.Remote()}
	if checkout := source.Checkout; checkout != nil {
		if checkout.RefType != nil {
			out.RefType = *checkout.RefType
		}
		if checkout.RefName != nil {
			out.RefName = *checkout.RefName
		}
		if checkout.Commit != nil {
			out.Commit = *checkout.Commit
		}
	}
	return out
}
