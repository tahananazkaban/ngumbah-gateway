// Package tenants is the gateway's client for the tenant service; it hides gRPC from resolvers.
package tenants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	tenantv1 "github.com/tahananazkaban/ngumbah-gateway/gen/ngumbah/tenant/v1"
)

// Plan is the tenant's subscription plan as the gateway understands it.
type Plan string

// Known plans exposed to clients.
const (
	PlanFree Plan = "FREE"
	PlanPro  Plan = "PRO"
)

// Tenant is a laundry business as returned to resolvers.
type Tenant struct {
	ID   string
	Name string
	Plan Plan
}

var (
	// ErrNotFound means the tenant service has no tenant with that ID.
	ErrNotFound = errors.New("tenant not found")
	// ErrInvalidID means the ID was rejected as malformed.
	ErrInvalidID = errors.New("invalid tenant id")
	// ErrUnavailable means the tenant service could not answer in time.
	ErrUnavailable = errors.New("tenant service unavailable")
	// ErrInternal means anything else went wrong; details stay in logs.
	ErrInternal = errors.New("tenant service internal error")
	// ErrCanceled means the caller gave up before the tenant service answered.
	ErrCanceled = errors.New("request canceled")
)

// Dial creates a lazily connecting, traced gRPC connection to the tenant service.
func Dial(target string, extra ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	}, extra...)
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial tenant service: %w", err)
	}
	return conn, nil
}

// Client calls the tenant service with a per-call timeout.
type Client struct {
	tenants tenantv1.TenantServiceClient
	health  healthpb.HealthClient
	timeout time.Duration
}

// NewClient wraps a connection; timeout bounds every call.
func NewClient(conn *grpc.ClientConn, timeout time.Duration) *Client {
	return &Client{
		tenants: tenantv1.NewTenantServiceClient(conn),
		health:  healthpb.NewHealthClient(conn),
		timeout: timeout,
	}
}

// Get returns the tenant or one of the package's error values wrapping the gRPC cause.
func (c *Client) Get(ctx context.Context, id string) (Tenant, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.tenants.GetTenant(ctx, &tenantv1.GetTenantRequest{TenantId: id})
	if err != nil {
		return Tenant{}, fromStatus(err)
	}
	t := resp.GetTenant()
	plan, ok := plans[t.GetPlan()]
	if !ok {
		return Tenant{}, fmt.Errorf("%w: unexpected plan %s", ErrInternal, t.GetPlan())
	}
	return Tenant{ID: t.GetId(), Name: t.GetName(), Plan: plan}, nil
}

// Status reports whether the tenant service is serving, plus a label for the readiness body.
func (c *Client) Status(ctx context.Context, timeout time.Duration) (serving bool, label string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := c.health.Check(ctx, &healthpb.HealthCheckRequest{Service: tenantv1.TenantService_ServiceDesc.ServiceName})
	if err != nil {
		return false, "UNREACHABLE"
	}
	return resp.GetStatus() == healthpb.HealthCheckResponse_SERVING, resp.GetStatus().String()
}

var plans = map[tenantv1.Plan]Plan{
	tenantv1.Plan_PLAN_FREE: PlanFree,
	tenantv1.Plan_PLAN_PRO:  PlanPro,
}

// fromStatus converts gRPC codes into the package's errors, keeping the cause for logs.
func fromStatus(err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %w", ErrInvalidID, err)
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	case codes.Canceled:
		return fmt.Errorf("%w: %w", ErrCanceled, err)
	default:
		return fmt.Errorf("%w: %w", ErrInternal, err)
	}
}
