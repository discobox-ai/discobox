// Package gateways implements a project's gateways (ADR 26-10-16-470): routes
// that send a provider's API through another origin, which every discobox of
// the project is routed through by its pool's proxy, and the server
// environment values their headers take.
package gateways

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
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

// Configured is the server configuration file's gateways (ADR 26-10-17-271):
// those it keeps on the default user's default project, and those it keeps on
// the projects it names by ID.
type Configured struct {
	Default   []gateway.Route
	ByProject map[string][]gateway.Route
}

type Service struct {
	store     *store.Store
	sandboxes ProxyPolicy
	// configured is the server configuration file's gateways, and
	// defaultUserID the user whose default project its default entry names.
	configured    Configured
	defaultUserID string
	// lookupEnv reads the server's environment; a test sets its own.
	lookupEnv func(string) (string, bool)
}

func NewService(store *store.Store, sandboxes ProxyPolicy, configured Configured, defaultUserID string) *Service {
	return &Service{store: store, sandboxes: sandboxes, configured: configured, defaultUserID: defaultUserID, lookupEnv: os.LookupEnv}
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
	// A value from the server's environment is the operator's to hand out,
	// and only the server configuration file can (ADR 26-10-17-271 §3): a
	// member naming a variable could send another project's key anywhere.
	if env := route.Env(); len(env) > 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "header values from the server's environment ("+env[0]+") are set only in the server configuration file; give the header a value")
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
		if errors.Is(err, store.ErrGatewayConflict) || errors.Is(err, store.ErrGatewayManaged) {
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
	if errors.Is(err, store.ErrGatewayManaged) {
		return apperrors.NewStatusError(http.StatusConflict, err.Error())
	}
	return err
}

// ApplyConfiguredGatewaysTx converges every project's configuration-managed
// gateways onto the server configuration file (ADR 26-10-17-271 §3), in the
// caller's transaction: the projects it names get exactly the gateways it
// names, every other project loses the ones it kept there, and a gateway the
// API made with a header from the server's environment is deleted. It runs at
// startup, and in the transaction that moves the default project, so what the
// default entry names is read where it is written. It is idempotent.
//
// A project ID that names no project is skipped with a warning (§4), as is the
// default entry when the default project is one the file also names by ID:
// the entry under its ID applies (§2).
func (s *Service) ApplyConfiguredGatewaysTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB) error {
	targets := map[string][]gateway.Route{}
	for projectID, routes := range s.configured.ByProject {
		if _, err := txStore.GetProject(ctx, projectID); errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "the server configuration file names a project that does not exist; its gateways are skipped", "project", projectID)
			continue
		} else if err != nil {
			return fmt.Errorf("projects.%s: %w", projectID, err)
		}
		targets[projectID] = routes
	}
	if len(s.configured.Default) > 0 {
		project, err := txStore.GetDefaultProjectForUser(ctx, s.defaultUserID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			slog.WarnContext(ctx, "the server configuration file names the default project, and there is none; its gateways are skipped")
		case err != nil:
			return fmt.Errorf("projects.default: %w", err)
		default:
			if _, byID := targets[project.ID]; byID {
				slog.WarnContext(ctx, "the server configuration file names the default project by its ID too; the entry under its ID applies", "project", project.ID)
			} else {
				targets[project.ID] = s.configured.Default
			}
		}
	}
	kept, err := txStore.ListProjectsToConverge(ctx)
	if err != nil {
		return err
	}
	for _, projectID := range kept {
		if _, ok := targets[projectID]; !ok {
			targets[projectID] = nil
		}
	}
	projectIDs := make([]string, 0, len(targets))
	for projectID := range targets {
		projectIDs = append(projectIDs, projectID)
	}
	sort.Strings(projectIDs)
	for _, projectID := range projectIDs {
		result, err := txStore.ConvergeConfiguredGateways(ctx, projectID, targets[projectID])
		if err != nil {
			return fmt.Errorf("project %s: %w", projectID, err)
		}
		if !result.Changed {
			continue
		}
		if err := s.sandboxes.MarkProxyPolicyChangedTx(ctx, txStore, txDB, projectID, "", ""); err != nil {
			return err
		}
		for _, name := range result.TakenOver {
			slog.InfoContext(ctx, "the server configuration file keeps a gateway the API made", "project", projectID, "gateway", name)
		}
		for _, gw := range result.Removed {
			slog.WarnContext(ctx, "deleting a gateway the API made: the server configuration file names another with its match", "project", projectID, "gateway", gw.Name, "match", gw.Match)
		}
		for _, gw := range result.FromEnv {
			slog.WarnContext(ctx, "deleting a gateway the API made with a header from the server's environment, which only the server configuration file may name", "project", projectID, "gateway", gw.Name, "match", gw.Match)
		}
	}
	return nil
}

// ResolveGatewayHeaders answers a pool's proxy with the server environment
// values a sandbox's gateway sets headers from (§4). The pool must host the
// sandbox, the gateway must be one the server configuration file keeps, and
// every variable asked for must be one the gateway, as it now stands, names:
// a pool can ask for nothing else of the server's environment.
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
	// Only a gateway the server configuration file keeps takes a value from
	// the environment (ADR 26-10-17-271 §3).
	if gw.ManagedBy != model.GatewayManagedByConfig {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "gateway "+name+" is not kept by the server configuration file, and takes no header from the server's environment")
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
