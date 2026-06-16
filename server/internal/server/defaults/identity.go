package defaults

import (
	"context"
	"time"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/database"
)

// InitializeIdentity ensures the built-in default tenant and user exist.
func InitializeIdentity(ctx context.Context, resolver *database.Resolver, tenantID, userID string) error {
	global, err := resolver.ResolveGlobal(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := global.Write.WithContext(ctx).Save(&model.Tenant{
		ID:        tenantID,
		Name:      "Default Tenant",
		Slug:      "default",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error; err != nil {
		return err
	}
	return global.Write.WithContext(ctx).Save(&model.User{
		ID:        userID,
		TenantID:  tenantID,
		Email:     "local@example.com",
		Provider:  "default",
		Subject:   "default",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error
}
