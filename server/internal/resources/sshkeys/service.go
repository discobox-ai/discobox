// Package sshkeys implements the project-scoped SSH key resource (ADR 0024
// §5): a managed public key that authorizes SSH connections to a project's
// sandboxes, distinct from the server-wide authorized_keys file.
package sshkeys

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store *store.Store
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) ListSSHKeys(ctx context.Context, projectID string) ([]model.SSHKey, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	return s.store.ListSSHKeys(ctx, projectID)
}

func (s *Service) CreateSSHKey(ctx context.Context, projectID string, input services.CreateSSHKeyBody) (*model.SSHKey, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	publicKey, comment, err := parseAuthorizedKeyLine(input.PublicKey)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid SSH public key: "+err.Error())
	}
	name := strings.TrimSpace(input.Name.Or(""))
	key := &model.SSHKey{
		ProjectID:   projectID,
		Name:        name,
		PublicKey:   strings.TrimRight(string(ssh.MarshalAuthorizedKey(publicKey)), "\n"),
		Fingerprint: ssh.FingerprintSHA256(publicKey),
		Comment:     comment,
	}
	if err := s.store.CreateSSHKey(ctx, key); err != nil {
		return nil, err
	}
	return s.store.GetSSHKey(ctx, projectID, key.ID)
}

func (s *Service) DeleteSSHKey(ctx context.Context, projectID, keyID string) error {
	if err := s.store.DeleteSSHKey(ctx, projectID, keyID); err != nil {
		return apiError(err, "SSH key not found")
	}
	return nil
}

// parseAuthorizedKeyLine parses a single authorized_keys(5) line (options
// blocks are not supported: this ingress never needs a forced command or
// source restriction) and returns the key and its trailing comment.
func parseAuthorizedKeyLine(line string) (ssh.PublicKey, string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, "", errors.New("public key is required")
	}
	pub, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, "", err
	}
	if len(strings.TrimSpace(string(rest))) > 0 {
		return nil, "", errors.New("only one public key may be provided")
	}
	return pub, comment, nil
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
