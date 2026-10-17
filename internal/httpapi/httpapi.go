// Package httpapi is the gateway's HTTP surface: GraphQL, health endpoints and cross-cutting middleware.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"

	"github.com/tahananazkaban/ngumbah-gateway/internal/graph"
	"github.com/tahananazkaban/ngumbah-gateway/internal/graph/generated"
	"github.com/tahananazkaban/ngumbah-gateway/internal/telemetry"
)

const (
	maxBodyBytes    = 1 << 20
	complexityLimit = 100
	readyTimeout    = time.Second
)

// TenantService is what the HTTP layer needs from the tenant service client.
type TenantService interface {
	graph.TenantGetter
	Status(ctx context.Context, timeout time.Duration) (serving bool, label string)
}

// Options wires the gateway's dependencies.
type Options struct {
	Tenants     TenantService
	Development bool
	CORSOrigins []string
	Logger      *slog.Logger
}

// New returns the full gateway handler, exactly as served in production.
func New(opts Options) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()
	mux.Handle("/graphql", graphqlHandler(opts, logger))
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", readiness(opts.Tenants))
	if opts.Development {
		mux.Handle("GET /playground", playground.Handler("Ngumbah gateway", "/graphql"))
	}

	// Outermost first: tracing, then trace header, logging, CORS, body limit
	var h http.Handler = mux
	h = limitBody(h)
	h = cors(opts.CORSOrigins, h)
	h = logRequests(logger, h)
	h = traceHeader(h)
	return otelhttp.NewHandler(h, "gateway", otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
		return r.Method + " " + r.URL.Path
	}))
}

func graphqlHandler(opts Options, logger *slog.Logger) http.Handler {
	srv := handler.New(generated.NewExecutableSchema(generated.Config{
		Resolvers: &graph.Resolver{Tenants: opts.Tenants, Logger: logger},
	}))
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	srv.Use(extension.FixedComplexityLimit(complexityLimit))
	if opts.Development {
		srv.Use(extension.Introspection{})
	}
	srv.AroundOperations(func(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
		if holder, ok := ctx.Value(operationKey{}).(*string); ok {
			oc := graphql.GetOperationContext(ctx)
			*holder = oc.OperationName
			// Clients often send only the query; take the name from the parsed operation
			if *holder == "" && oc.Operation != nil {
				*holder = oc.Operation.Name
			}
		}
		return next(ctx)
	})
	srv.SetErrorPresenter(func(ctx context.Context, err error) *gqlerror.Error {
		// Errors built by resolvers already carry a safe code and message
		var gqlErr *gqlerror.Error
		if errors.As(err, &gqlErr) && gqlErr.Extensions != nil {
			return gqlErr
		}
		return graphql.DefaultErrorPresenter(ctx, err)
	})
	srv.SetRecoverFunc(func(ctx context.Context, p any) error {
		logger.ErrorContext(ctx, "resolver panic", append([]any{"panic", p}, telemetry.TraceArgs(ctx)...)...)
		return graph.NewError(ctx, graph.CodeInternal, "internal error")
	})
	return srv
}

func readiness(t TenantService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serving, label := t.Status(r.Context(), readyTimeout)
		code, state := http.StatusOK, "ok"
		if !serving {
			code, state = http.StatusServiceUnavailable, "unavailable"
		}
		writeJSON(w, code, map[string]any{"status": state, "dependencies": map[string]string{"tenant-svc": label}})
	}
}

// limitBody answers 413 for bodies over the limit, whether or not their length is declared.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tooLarge := func() {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		}
		if r.ContentLength > maxBodyBytes {
			tooLarge()
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			tooLarge()
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// cors allows only configured origins and answers preflight requests itself.
func cors(origins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && slices.Contains(origins, origin)
		if allowed {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "X-Trace-Id")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if allowed {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Traceparent")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// traceHeader returns the request's trace ID so frontend errors can be matched to logs.
func traceHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			w.Header().Set("X-Trace-Id", sc.TraceID().String())
		}
		next.ServeHTTP(w, r)
	})
}

type operationKey struct{}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logRequests writes one line per request with status, duration, GraphQL operation and trace IDs.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var operation string
		r = r.WithContext(context.WithValue(r.Context(), operationKey{}, &operation))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		args := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if operation != "" {
			args = append(args, "operation", operation)
		}
		args = append(args, telemetry.TraceArgs(r.Context())...)
		level := slog.LevelInfo
		// Health probes run every few seconds; keep them out of normal logs
		if r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}
		logger.Log(r.Context(), level, "http", args...)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
