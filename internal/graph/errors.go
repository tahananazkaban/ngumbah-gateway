package graph

import (
	"context"
	"errors"

	"github.com/99designs/gqlgen/graphql"
	"github.com/tahananazkaban/ngumbah-gateway/internal/telemetry"
	"github.com/tahananazkaban/ngumbah-gateway/internal/tenants"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// Error codes returned in extensions.code; the web app switches on these.
const (
	CodeNotFound           = "NOT_FOUND"
	CodeBadUserInput       = "BAD_USER_INPUT"
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
	CodeInternal           = "INTERNAL"
)

// NewError builds a client-facing GraphQL error with a stable code.
func NewError(ctx context.Context, code, message string) *gqlerror.Error {
	return &gqlerror.Error{
		Message:    message,
		Path:       graphql.GetPath(ctx),
		Extensions: map[string]any{"code": code},
	}
}

// toGraphQLError maps downstream errors to codes and logs the details that clients must not see.
func (r *Resolver) toGraphQLError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, tenants.ErrNotFound):
		return NewError(ctx, CodeNotFound, "tenant not found")
	case errors.Is(err, tenants.ErrInvalidID):
		return NewError(ctx, CodeBadUserInput, "id must be a UUID")
	case errors.Is(err, tenants.ErrCanceled):
		// The client is gone; nothing to fix server-side
		r.Logger.InfoContext(ctx, "request canceled by client", telemetry.TraceArgs(ctx)...)
		return NewError(ctx, CodeServiceUnavailable, "request canceled")
	case errors.Is(err, tenants.ErrUnavailable):
		r.logError(ctx, "downstream unavailable", err)
		return NewError(ctx, CodeServiceUnavailable, "service temporarily unavailable, try again")
	default:
		r.logError(ctx, "downstream failed", err)
		return NewError(ctx, CodeInternal, "internal error")
	}
}

// logError keeps the full error server-side, tagged with the request's trace.
func (r *Resolver) logError(ctx context.Context, msg string, err error) {
	r.Logger.ErrorContext(ctx, msg, append([]any{"error", err}, telemetry.TraceArgs(ctx)...)...)
}
