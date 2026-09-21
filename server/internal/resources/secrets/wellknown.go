package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
)

// This file is the control plane's side of well-known credentials: an ask that
// names one by ID, and the secret that fulfills it when a person approves.

// wellKnownAsk checks an ask that names a well-known credential and returns the
// name, variable, and host it names. What the ask spells out itself must agree
// with the ID: an ID is a promise about what is asked for, and an ask that says
// otherwise is one of the two, not both.
func wellKnownAsk(id, name, envName, host string) (string, string, string, error) {
	known, ok := wellknown.Lookup(strings.TrimSpace(id))
	if !ok {
		return "", "", "", apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("%q is not a well-known credential", id))
	}
	conflict := func(field, got, want string) error {
		return apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("%s is %s, whose %s is %q, not %q", id, known.Name, field, want, got))
	}
	if name != "" && name != known.Name {
		return "", "", "", conflict("name", name, known.Name)
	}
	if envName != "" && envName != known.EnvVar {
		return "", "", "", conflict("variable", envName, known.EnvVar)
	}
	if host == "" {
		host = known.Host()
	} else if !known.AllowsHost(host) {
		return "", "", "", conflict("host", host, known.Host())
	}
	return known.Name, known.EnvVar, host, nil
}

// wellKnownSecret is the secret an approval of a request for a well-known
// credential binds. chosenID is the secret the approver named, if any.
//
// With none named it is the secret marked with the ID. Naming one answers
// this request; whether it also becomes the mark is decided only once the
// approval has gone through (see ApproveSecretRequest).
func (s *Service) wellKnownSecret(ctx context.Context, projectID, id, chosenID string) (*model.Secret, error) {
	if _, ok := wellknown.Lookup(id); !ok {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("%q is not a well-known credential", id))
	}
	if chosenID != "" {
		secret, err := s.store.GetSecret(ctx, projectID, chosenID)
		if err != nil {
			return nil, apperrors.NotFound(err, "secret not found")
		}
		return secret, nil
	}
	marked, err := s.store.FindSecretByWellKnownID(ctx, projectID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("no secret fulfills %s yet: name the secret that does, and it will answer every later request for it", id))
	}
	return marked, err
}
