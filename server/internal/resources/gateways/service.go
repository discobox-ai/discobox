// Package gateways implements a project's gateways (ADR 26-10-16-470): routes
// that send a provider's API through another origin, which every discobox of
// the project is routed through by its pool's proxy, and the server
// environment values their headers take.
package gateways

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// ProxyPolicy converges a project's discoboxes onto its changed gateways. The
// sandbox service satisfies it.
type ProxyPolicy interface {
	MarkProxyPolicyChangedTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID, poolID, harnessConfigID string) error
}

type Service struct {
	store     *store.Store
	sandboxes ProxyPolicy
	// lookupEnv reads the server's environment; a test sets its own.
	lookupEnv func(string) (string, bool)
}

func NewService(store *store.Store, sandboxes ProxyPolicy) *Service {
	return &Service{store: store, sandboxes: sandboxes, lookupEnv: os.LookupEnv}
}

func (s *Service) ListProjectGateways(ctx context.Context, projectID string) ([]model.ProjectGateway, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	return s.store.ListProjectGateways(ctx, projectID)
}

func (s *Service) GetProjectGateway(ctx context.Context, projectID, name string) (*model.ProjectGateway, error) {
	gw, err := s.store.GetProjectGateway(ctx, projectID, name)
	if err != nil {
		return nil, apperrors.NotFound(err, "gateway not found")
	}
	return gw, nil
}

// PutProjectGateway creates the gateway or replaces the one of its name, and
// in the same transaction marks every discobox of the project for the push
// that delivers it (§5).
func (s *Service) PutProjectGateway(ctx context.Context, projectID, name string, route gateway.Route) (*model.ProjectGateway, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	route.Name = name
	route = gateway.Normalize(route)
	if err := route.Validate(); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	gw := &model.ProjectGateway{
		ProjectID:      projectID,
		Name:           route.Name,
		Match:          route.Match,
		Target:         route.Target,
		RequestHeaders: route.RequestHeaders,
	}
	if principal, ok := auth.PrincipalFromContext(ctx); ok {
		gw.CreatedBy = principal.UserID
	}
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.PutProjectGateway(ctx, gw); err != nil {
			return err
		}
		return s.sandboxes.MarkProxyPolicyChangedTx(ctx, txStore, txDB, projectID, "", "")
	}); err != nil {
		if errors.Is(err, store.ErrGatewayConflict) {
			return nil, apperrors.NewStatusError(http.StatusConflict, err.Error())
		}
		return nil, err
	}
	return s.store.GetProjectGateway(ctx, projectID, route.Name)
}

// DeleteProjectGateway removes the gateway and marks every discobox of the
// project for the push that takes it away.
func (s *Service) DeleteProjectGateway(ctx context.Context, projectID, name string) error {
	err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.DeleteProjectGateway(ctx, projectID, name); err != nil {
			return err
		}
		return s.sandboxes.MarkProxyPolicyChangedTx(ctx, txStore, txDB, projectID, "", "")
	})
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NotFound(err, "gateway not found")
	}
	return err
}

// ResolveGatewayHeaders answers a pool's proxy with the server environment
// values a sandbox's gateway sets headers from (§4). The pool must host the
// sandbox, and every variable asked for must be one the gateway, as it now
// stands, names: a pool can ask for nothing else of the server's environment.
// A variable the environment does not set, or sets empty, is left out of the
// answer.
func (s *Service) ResolveGatewayHeaders(ctx context.Context, poolID, sandboxID, name string, env []string) (map[string]string, error) {
	sb, err := s.store.GetSandboxByID(ctx, sandboxID)
	if err != nil || strings.TrimSpace(sb.PoolID) != strings.TrimSpace(poolID) {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "sandbox not found")
	}
	gw, err := s.store.GetProjectGateway(ctx, sb.ProjectID, name)
	if err != nil {
		return nil, apperrors.NotFound(err, "gateway not found")
	}
	named := gw.Route().Env()
	values := make(map[string]string, len(env))
	for _, variable := range env {
		if !slices.Contains(named, variable) || gateway.ValidateEnv(variable) != nil {
			return nil, apperrors.NewStatusError(http.StatusForbidden, "gateway "+name+" takes no header from "+variable)
		}
		if value, ok := s.lookupEnv(variable); ok && value != "" {
			values[variable] = value
		}
	}
	return values, nil
}
