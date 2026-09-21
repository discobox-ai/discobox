package secrets

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// grantPurpose reads what a grant is asked to authorize. Nothing named is a use
// grant, which is every grant there was before delegation.
func grantPurpose(purpose string) (string, error) {
	switch purpose = strings.TrimSpace(purpose); purpose {
	case "", model.SecretGrantPurposeUse:
		return model.SecretGrantPurposeUse, nil
	case model.SecretGrantPurposeDelegate:
		return purpose, nil
	}
	return "", apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("a grant's purpose is use or delegate, not %q", purpose))
}

// guardPurpose refuses a delegation grant that does not hold together. It runs
// in mintGrantAs, the one place every grant passes through, beside
// guardGrantHost and guardGrantTTL, for their reason: the checks have to bind
// every path that mints, not the one that happened to remember them.
//
// A delegation grant lets one discobox delegate the credential and lets it do
// nothing else: it authorizes nothing its holder sends.
func (s *Service) guardPurpose(ctx context.Context, projectID, scope, scopeKey string, uses []model.SecretUse, purpose string) error {
	if purpose != model.SecretGrantPurposeDelegate {
		return nil
	}
	// Delegation is one discobox's, a use at a time: a grant wider than a
	// discobox has nobody in particular to delegate it, and one without uses
	// has nothing a delegation could name.
	if scope != model.SecretGrantScopeSandbox || len(uses) == 0 {
		return apperrors.NewStatusError(http.StatusBadRequest, "only a grant held by one discobox, carrying uses, may be a delegation grant")
	}
	if _, err := s.store.GetSandbox(ctx, projectID, scopeKey); err != nil {
		return apperrors.NotFound(err, "sandbox not found")
	}
	return nil
}
