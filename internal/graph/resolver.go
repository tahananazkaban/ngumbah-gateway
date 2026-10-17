// Package graph resolves GraphQL fields by calling downstream services.
package graph

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tahananazkaban/ngumbah-gateway/internal/graph/model"
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

// resolveTenant backs Query.tenant; kept here because gqlgen marks its own file as generated.
func (r *Resolver) resolveTenant(ctx context.Context, id string) (*model.Tenant, error) {
	t, err := r.Tenants.Get(ctx, id)
	if err != nil {
		return nil, r.toGraphQLError(ctx, err)
	}
	plan, ok := modelPlans[t.Plan]
	if !ok {
		return nil, r.toGraphQLError(ctx, fmt.Errorf("%w: no GraphQL plan for %q", tenants.ErrInternal, t.Plan))
	}
	return &model.Tenant{ID: t.ID, Name: t.Name, Plan: plan}, nil
}

var modelPlans = map[tenants.Plan]model.Plan{
	tenants.PlanFree: model.PlanFree,
	tenants.PlanPro:  model.PlanPro,
}
