package tenantctx

import (
	"context"
	"fmt"
	"strings"
)

type tenantIDKey struct{}

// WithTenantID returns a context scoped to tenantID.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey{}, strings.TrimSpace(tenantID))
}

// TenantID returns the tenant ID carried by ctx.
func TenantID(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("tenant ID is required")
	}
	tenantID, _ := ctx.Value(tenantIDKey{}).(string)
	if tenantID == "" {
		return "", fmt.Errorf("tenant ID is required")
	}
	return tenantID, nil
}
