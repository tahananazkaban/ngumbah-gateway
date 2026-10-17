// Package graph resolves GraphQL fields by calling downstream services.
package graph

import (
	"context"
	"log/slog"

	"github.com/tahananazkaban/ngumbah-gateway/internal/tenants"
)

// TenantGetter is what resolvers need from the tenant service.
type TenantGetter interface {
	Get(ctx context.Context, id string) (tenants.Tenant, error)
}

// Resolver holds the downstream clients shared by all resolvers.
type Resolver struct {
	Tenants TenantGetter
	Logger  *slog.Logger
}
