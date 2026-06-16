package defaults

import (
	"context"
	"time"

	"github.com/obot-platform/discobox/model"
	"gorm.io/gorm"
)

// InitializeIdentity ensures the built-in default user exists.
func InitializeIdentity(ctx context.Context, db *gorm.DB, userID string) error {
	now := time.Now().UTC()
	return db.WithContext(ctx).Save(&model.User{
		ID:        userID,
		Email:     "local@example.com",
		Provider:  "default",
		Subject:   "default",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error
}
