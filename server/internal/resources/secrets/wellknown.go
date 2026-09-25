package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apigen "github.com/discobox-ai/discobox/api/gen"
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
	known, ok := wellknown.Lookup(id)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("%q is not a well-known credential", id))
	}
	if known.Gate {
		if chosenID != "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("%s has no secret to choose: approve it without naming one", id))
		}
		return s.gateSecret(ctx, projectID, known)
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
			fmt.Sprintf("no secret fulfills %s yet: name the secret that does, and it will answer every later request for it; `discobox secret create --well-known %s` stores one", id, id))
	}
	return marked, err
}

// gateSecret is the project's secret for a gate credential, created the first
// time a request for it is approved. Grants and bindings are always of a
// secret, so a gate has one; nothing about it is a credential. Its value is a
// random string no upstream knows, it is never resolved (ResolveSandboxSecret),
// and the pool admits a request carrying a use of it rather than swapping
// anything in (ADR 0140 §§1–2).
func (s *Service) gateSecret(ctx context.Context, projectID string, known wellknown.Credential) (*model.Secret, error) {
	if marked, err := s.store.FindSecretByWellKnownID(ctx, projectID, known.ID); err == nil {
		return marked, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	filler := make([]byte, 32)
	if _, err := rand.Read(filler); err != nil {
		return nil, err
	}
	value, err := marshalSecretValue(apigen.SecretValue{Token: apigen.NewOptString(hex.EncodeToString(filler))})
	if err != nil {
		return nil, err
	}
	gate := &model.Secret{
		ProjectID:      projectID,
		Name:           known.ID,
		Type:           model.SecretTypeToken,
		Host:           known.Host(),
		WellKnownID:    known.ID,
		EncryptedValue: value,
	}
	if err := s.store.CreateSecret(ctx, gate); err != nil {
		// Two first approvals: the partial unique index keeps one, and that
		// one is the gate.
		if marked, findErr := s.store.FindSecretByWellKnownID(ctx, projectID, known.ID); findErr == nil {
			return marked, nil
		}
		return nil, err
	}
	return gate, nil
}

// isGateSecret reports whether a secret stands for a gate credential, whose
// value is never handed out.
func isGateSecret(secret *model.Secret) bool {
	known, ok := wellknown.Lookup(secret.WellKnownID)
	return ok && known.Gate
}

// reservedHostAsk refuses a free-form ask for a gate credential's host. Only
// an ask by its ID may open a gate: a secret approved for that host under
// another name would admit its holder to the discobox API unannounced.
func reservedHostAsk(wellKnownID, host string) error {
	for _, known := range wellknown.All() {
		if known.Gate && known.ID != wellKnownID && known.AllowsHost(host) {
			return apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("%s is reached only through %s: ask for it by that ID", host, known.ID))
		}
	}
	return nil
}

// answersWellKnown makes a new secret the one that answers a well-known ID,
// as a person creating it for that ID means it to be: the first request for
// the ID then binds it without asking which secret answers. It is the mark the
// first approval would otherwise set, set ahead of any request. A gate is not
// created this way — its secret is made by approving a request for it — and
// an ID another secret already answers stays with that secret until somebody
// deletes it.
func (s *Service) answersWellKnown(ctx context.Context, sec *model.Secret, id string) error {
	known, ok := wellknown.Lookup(id)
	if !ok {
		return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("%q is not a well-known credential", id))
	}
	if known.Gate {
		return apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("%s is a gate, with no value: it is given by approving a request for it", id))
	}
	if sec.Type != model.SecretTypeToken {
		return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("%s is answered by a token", id))
	}
	existing, err := s.store.FindSecretByWellKnownID(ctx, sec.ProjectID, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if existing != nil {
		return apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("%s already answers %s; delete it, or store this one without the ID", existing.Name, id))
	}
	sec.WellKnownID = id
	return nil
}
