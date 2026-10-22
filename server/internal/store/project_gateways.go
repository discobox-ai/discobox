package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// ErrGatewayConflict is a gateway whose match another gateway of the project
// already has: a request would then match two (ADR 26-10-16-470 §2).
var ErrGatewayConflict = errors.New("gateway conflicts with the project's gateways")

// ErrGatewayManaged is a change through the API to a gateway the server
// configuration file keeps, or to one that would take its match
// (ADR 26-10-17-271 §3).
var ErrGatewayManaged = errors.New("gateway is kept by the server configuration file (server.yaml)")

// ListProjectGateways returns a project's gateways by name.
func (s *Store) ListProjectGateways(ctx context.Context, projectID string) ([]model.ProjectGateway, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.ProjectGateway
	err = read.Where("project_id = ?", projectID).Order("name ASC").Find(&out).Error
	return out, err
}

// GetProjectGateway returns a project's gateway by name.
func (s *Store) GetProjectGateway(ctx context.Context, projectID, name string) (*model.ProjectGateway, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out model.ProjectGateway
	err = read.Where("project_id = ? AND name = ?", projectID, name).Take(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PutProjectGateway creates the gateway through the API, or replaces the one
// of its name in place, keeping its ID, creator, and creation time. It holds
// the project's row for update first, so two puts at once take turns, and a
// put takes turns with the configuration file's (ConvergeConfiguredGateways):
// on a database with more than one writer the second sees the first's match,
// and the unique index alone would answer it with an index violation rather
// than the conflict.
func (s *Store) PutProjectGateway(ctx context.Context, gw *model.ProjectGateway) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Transaction(func(tx *gorm.DB) error {
		if err := lockProject(tx, gw.ProjectID); err != nil {
			return err
		}
		// The name first: a gateway the file keeps is the file's whatever its
		// match collides with, and the refusal should say so.
		var existing model.ProjectGateway
		err := tx.Where("project_id = ? AND name = ?", gw.ProjectID, gw.Name).Take(&existing).Error
		found := err == nil
		switch {
		case found && existing.ManagedBy == model.GatewayManagedByConfig:
			return fmt.Errorf("%w: gateway %s", ErrGatewayManaged, gw.Name)
		case !found && !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		}
		var other model.ProjectGateway
		err = tx.Where("project_id = ? AND match_url = ? AND name <> ?", gw.ProjectID, gw.Match, gw.Name).Take(&other).Error
		switch {
		case err == nil && other.ManagedBy == model.GatewayManagedByConfig:
			return fmt.Errorf("%w: gateway %s already matches %s", ErrGatewayManaged, other.Name, gw.Match)
		case err == nil:
			return fmt.Errorf("%w: gateway %s already matches %s", ErrGatewayConflict, other.Name, gw.Match)
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		}
		gw.ManagedBy = model.GatewayManagedByAPI
		if !found {
			return tx.Create(gw).Error
		}
		gw.ID, gw.CreatedBy, gw.CreatedAt = existing.ID, existing.CreatedBy, existing.CreatedAt
		return tx.Select("match_url", "target_url", "request_headers", "updated_at").Save(gw).Error
	})
}

// DeleteProjectGateway removes a project's gateway by name, through the API:
// one the configuration file keeps is refused.
func (s *Store) DeleteProjectGateway(ctx context.Context, projectID, name string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Where("project_id = ? AND name = ? AND managed_by = ?", projectID, name, model.GatewayManagedByAPI).Delete(&model.ProjectGateway{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		return nil
	}
	if _, err := s.GetProjectGateway(ctx, projectID, name); err != nil {
		return err
	}
	return fmt.Errorf("%w: gateway %s", ErrGatewayManaged, name)
}

// GatewayConvergence is what ConvergeConfiguredGateways did to a project's
// gateways that the API had made, which the caller reports.
type GatewayConvergence struct {
	// Changed is whether any gateway of the project changed.
	Changed bool
	// TakenOver are the API's gateways the file now keeps, under their name.
	TakenOver []string
	// Removed are the API's gateways deleted because the file names another
	// with their match.
	Removed []model.ProjectGateway
	// FromEnv are the API's gateways deleted because a header of theirs takes
	// its value from the server's environment, which only the file may name.
	FromEnv []model.ProjectGateway
}

// ConvergeConfiguredGateways makes a project's configuration-managed gateways
// exactly routes (ADR 26-10-17-271 §3), holding the project's row as a put
// does. A gateway the file names replaces the one of its name, whoever made
// it; a gateway the API made with the match of one the file names is deleted;
// a gateway the file kept and no longer names is deleted, and so is one the
// API made with a header from the server's environment, which only the file
// may name (§3). The API's other gateways are left alone. A gateway already as the file says is not
// rewritten, so a startup that changes nothing changes no row.
func (s *Store) ConvergeConfiguredGateways(ctx context.Context, projectID string, routes []gateway.Route) (GatewayConvergence, error) {
	var out GatewayConvergence
	write, err := s.getWrite(ctx)
	if err != nil {
		return out, err
	}
	err = write.Transaction(func(tx *gorm.DB) error {
		if err := lockProject(tx, projectID); err != nil {
			return err
		}
		var existing []model.ProjectGateway
		if err := tx.Where("project_id = ?", projectID).Find(&existing).Error; err != nil {
			return err
		}
		desired := make(map[string]gateway.Route, len(routes))
		matches := make(map[string]bool, len(routes))
		for _, route := range routes {
			desired[route.Name] = route
			matches[route.Match] = true
		}
		previous := map[string]model.ProjectGateway{}
		for _, gw := range existing {
			route, named := desired[gw.Name]
			config := gw.ManagedBy == model.GatewayManagedByConfig
			switch {
			case named && config && sameRoute(gw, route):
				delete(desired, gw.Name)
				continue
			case named && config:
				previous[gw.Name] = gw
			case named:
				out.TakenOver = append(out.TakenOver, gw.Name)
			case matches[gw.Match] && !config:
				out.Removed = append(out.Removed, gw)
			case !config && len(gw.Route().Env()) > 0:
				out.FromEnv = append(out.FromEnv, gw)
			case !config:
				continue
			}
			// Deleted before any is written, so the unique indexes never see
			// a match or a name held twice on the way.
			if err := tx.Delete(&model.ProjectGateway{}, "id = ?", gw.ID).Error; err != nil {
				return err
			}
			out.Changed = true
		}
		for _, route := range routes {
			if _, ok := desired[route.Name]; !ok {
				continue
			}
			gw := &model.ProjectGateway{
				ProjectID:      projectID,
				Name:           route.Name,
				Match:          route.Match,
				Target:         route.Target,
				RequestHeaders: route.RequestHeaders,
				ManagedBy:      model.GatewayManagedByConfig,
			}
			if kept, ok := previous[route.Name]; ok {
				gw.ID, gw.CreatedAt = kept.ID, kept.CreatedAt
			}
			if err := tx.Create(gw).Error; err != nil {
				return err
			}
			out.Changed = true
		}
		return nil
	})
	return out, err
}

// ListProjectsToConverge returns the IDs of the projects ConvergeConfiguredGateways
// has something to do in whatever the file names: those holding a gateway the
// file keeps, which a file that stopped naming them still has to take away,
// and those holding one the API made with a header from the server's
// environment, from before only the file could name one.
func (s *Store) ListProjectsToConverge(ctx context.Context) ([]string, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	// An environment header is stored as {"name":...,"env":"..."}, and env is
	// written only when set.
	err = read.Model(&model.ProjectGateway{}).
		Where("managed_by = ? OR request_headers LIKE ?", model.GatewayManagedByConfig, `%"env":%`).
		Distinct().Order("project_id").Pluck("project_id", &out).Error
	return out, err
}

// lockProject holds a project's row for update, which is what serializes
// changes to its gateways.
func lockProject(tx *gorm.DB, projectID string) error {
	var project model.Project
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&project, "id = ?", projectID).Error
}

// sameRoute reports whether a stored gateway is already route. The headers
// are compared as they are stored, so an empty list and a missing one agree.
func sameRoute(gw model.ProjectGateway, route gateway.Route) bool {
	if gw.Match != route.Match || gw.Target != route.Target {
		return false
	}
	stored, err := json.Marshal(gw.RequestHeaders)
	if err != nil {
		return false
	}
	want, err := json.Marshal(route.RequestHeaders)
	return err == nil && bytes.Equal(stored, want)
}
