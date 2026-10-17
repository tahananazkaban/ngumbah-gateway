package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	tenantv1 "github.com/tahananazkaban/ngumbah-gateway/gen/ngumbah/tenant/v1"
	"github.com/tahananazkaban/ngumbah-gateway/internal/httpapi"
	"github.com/tahananazkaban/ngumbah-gateway/internal/telemetry"
	"github.com/tahananazkaban/ngumbah-gateway/internal/tenants"
)

const webOrigin = "http://localhost:5173"

func TestMain(m *testing.M) {
	shutdown := telemetry.Setup()
	code := m.Run()
	shutdown()
	os.Exit(code)
}

// fakeTenants is a real gRPC TenantService whose behavior each test sets.
type fakeTenants struct {
	tenantv1.UnimplementedTenantServiceServer
	mu          sync.Mutex
	get         func(id string) (*tenantv1.Tenant, error)
	traceparent string
}

func (f *fakeTenants) GetTenant(ctx context.Context, req *tenantv1.GetTenantRequest) (*tenantv1.GetTenantResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("traceparent"); len(v) > 0 {
			f.mu.Lock()
			f.traceparent = v[0]
			f.mu.Unlock()
		}
	}
	t, err := f.get(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	return &tenantv1.GetTenantResponse{Tenant: t}, nil
}

func (f *fakeTenants) receivedTraceparent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.traceparent
}

type harness struct {
	handler http.Handler
	fake    *fakeTenants
	health  *health.Server
}

type harnessOpts struct {
	development bool
	callTimeout time.Duration
	downstream  bool // false simulates an unreachable tenant service
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	fake := &fakeTenants{get: func(string) (*tenantv1.Tenant, error) { return nil, status.Error(codes.Internal, "unset") }}
	hs := health.NewServer()
	hs.SetServingStatus(tenantv1.TenantService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)

	lis := bufconn.Listen(1 << 20)
	if o.downstream {
		gs := grpc.NewServer()
		tenantv1.RegisterTenantServiceServer(gs, fake)
		healthpb.RegisterHealthServer(gs, hs)
		go func() { _ = gs.Serve(lis) }()
		t.Cleanup(gs.Stop)
	} else {
		_ = lis.Close()
	}

	conn, err := tenants.Dial("passthrough:///tenant-svc",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	timeout := o.callTimeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	handler := httpapi.New(httpapi.Options{
		Tenants:     tenants.NewClient(conn, timeout),
		Development: o.development,
		CORSOrigins: []string{webOrigin},
	})
	return &harness{handler: handler, fake: fake, health: hs}
}

type gqlResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func (h *harness) graphql(t *testing.T, query string, headers map[string]string) (*httptest.ResponseRecorder, gqlResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	var resp gqlResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return rec, resp
}

const tenantQuery = `query Greeting { tenant(id: "019a0000-0000-7000-8000-000000000001") { id name plan } }`

func errorCode(t *testing.T, resp gqlResponse) string {
	t.Helper()
	if len(resp.Errors) != 1 {
		t.Fatalf("want exactly one error, got %+v", resp.Errors)
	}
	code, _ := resp.Errors[0].Extensions["code"].(string)
	return code
}

func TestTenantFoundMapsFieldsAndPlan(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: true})
	for plan, want := range map[tenantv1.Plan]string{tenantv1.Plan_PLAN_FREE: "FREE", tenantv1.Plan_PLAN_PRO: "PRO"} {
		h.fake.get = func(id string) (*tenantv1.Tenant, error) {
			return &tenantv1.Tenant{Id: id, Name: "Laundry Demo", Plan: plan}, nil
		}
		rec, resp := h.graphql(t, tenantQuery, nil)
		if rec.Code != http.StatusOK || len(resp.Errors) != 0 {
			t.Fatalf("status %d errors %+v", rec.Code, resp.Errors)
		}
		var got struct{ ID, Name, Plan string }
		_ = json.Unmarshal(resp.Data["tenant"], &got)
		if got.ID != "019a0000-0000-7000-8000-000000000001" || got.Name != "Laundry Demo" || got.Plan != want {
			t.Errorf("plan %v: got %+v", plan, got)
		}
	}
}

func TestTenantErrorsMapToCodes(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		tenant   *tenantv1.Tenant
		wantCode string
	}{
		{"not found", status.Error(codes.NotFound, "tenant not found"), nil, "NOT_FOUND"},
		{"invalid id", status.Error(codes.InvalidArgument, "tenant_id must be a UUID"), nil, "BAD_USER_INPUT"},
		{"unavailable", status.Error(codes.Unavailable, "secret-host:5432 refused"), nil, "SERVICE_UNAVAILABLE"},
		{"internal", status.Error(codes.Internal, "relation secret_table missing"), nil, "INTERNAL"},
		{"unspecified plan", nil, &tenantv1.Tenant{Id: "x", Name: "y", Plan: tenantv1.Plan_PLAN_UNSPECIFIED}, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{downstream: true})
			h.fake.get = func(string) (*tenantv1.Tenant, error) { return tc.tenant, tc.err }

			_, resp := h.graphql(t, tenantQuery, nil)
			if got := errorCode(t, resp); got != tc.wantCode {
				t.Fatalf("code: got %q, want %q", got, tc.wantCode)
			}
			if string(resp.Data["tenant"]) != "null" {
				t.Errorf("tenant should be null, got %s", resp.Data["tenant"])
			}
			msg := resp.Errors[0].Message
			for _, leak := range []string{"secret", "5432", "relation", "PLAN_UNSPECIFIED"} {
				if strings.Contains(msg, leak) {
					t.Errorf("message leaks %q: %q", leak, msg)
				}
			}
		})
	}
}

func TestTenantSlowDownstreamIsServiceUnavailable(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: true, callTimeout: 100 * time.Millisecond})
	h.fake.get = func(string) (*tenantv1.Tenant, error) {
		time.Sleep(500 * time.Millisecond)
		return &tenantv1.Tenant{}, nil
	}
	_, resp := h.graphql(t, tenantQuery, nil)
	if got := errorCode(t, resp); got != "SERVICE_UNAVAILABLE" {
		t.Fatalf("got %q, want SERVICE_UNAVAILABLE", got)
	}
}

func TestTenantUnreachableDownstreamIsServiceUnavailable(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: false, callTimeout: 500 * time.Millisecond})
	_, resp := h.graphql(t, tenantQuery, nil)
	if got := errorCode(t, resp); got != "SERVICE_UNAVAILABLE" {
		t.Fatalf("got %q, want SERVICE_UNAVAILABLE", got)
	}
}

func get(h *harness, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func TestReadinessFollowsTenantService(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: true})
	if rec := get(h, "/readyz", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tenant-svc":"SERVING"`) {
		t.Fatalf("serving: %d %s", rec.Code, rec.Body.String())
	}
	h.health.SetServingStatus(tenantv1.TenantService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	if rec := get(h, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not serving: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReadinessUnreachableAndLivenessIndependent(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: false})
	if rec := get(h, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: got %d", rec.Code)
	}
	if rec := get(h, "/livez", nil); rec.Code != http.StatusOK {
		t.Fatalf("livez: got %d", rec.Code)
	}
}

func TestCORSAllowsOnlyConfiguredOrigin(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: true})
	preflight := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodOptions, "/graphql", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", "content-type")
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}
	if got := preflight(webOrigin).Header().Get("Access-Control-Allow-Origin"); got != webOrigin {
		t.Errorf("allowed origin: got %q", got)
	}
	if got := preflight("https://evil.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("other origin should get no CORS header, got %q", got)
	}
}

func TestCORSOnSimpleRequests(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: true})
	if got := get(h, "/livez", map[string]string{"Origin": webOrigin}).Header().Get("Access-Control-Allow-Origin"); got != webOrigin {
		t.Errorf("allowed origin: got %q", got)
	}
	if got := get(h, "/livez", map[string]string{"Origin": "https://evil.example"}).Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("other origin should get no CORS header, got %q", got)
	}
}

func TestOversizedChunkedBodyIsRejected(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: true})
	big := `{"query":"` + strings.Repeat("a", 2<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	// Unknown length, as with chunked transfer encoding
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: true})
	big := `{"query":"` + strings.Repeat("a", 2<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
}

func TestOverlyComplexQueryIsRejected(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: true})
	var b strings.Builder
	b.WriteString("{")
	for i := range 120 {
		b.WriteString("t")
		b.WriteString(strings.Repeat("x", i%26+1))
		b.WriteString(`: tenant(id: "019a0000-0000-7000-8000-000000000001") { id } `)
	}
	b.WriteString("}")
	_, resp := h.graphql(t, b.String(), nil)
	if len(resp.Errors) == 0 || !strings.Contains(strings.ToLower(resp.Errors[0].Message), "complexity") {
		t.Fatalf("want complexity error, got %+v", resp.Errors)
	}
}

func TestIntrospectionOnlyInDevelopment(t *testing.T) {
	q := `{ __schema { queryType { name } } }`
	dev := newHarness(t, harnessOpts{downstream: true, development: true})
	if _, resp := dev.graphql(t, q, nil); len(resp.Errors) != 0 {
		t.Fatalf("development: %+v", resp.Errors)
	}
	prod := newHarness(t, harnessOpts{downstream: true})
	if _, resp := prod.graphql(t, q, nil); len(resp.Errors) == 0 {
		t.Fatal("production: introspection should be rejected")
	}
	if rec := get(prod, "/playground", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("production playground: got %d", rec.Code)
	}
}

func TestTraceIsPropagatedAndReturned(t *testing.T) {
	h := newHarness(t, harnessOpts{downstream: true})
	h.fake.get = func(id string) (*tenantv1.Tenant, error) {
		return &tenantv1.Tenant{Id: id, Name: "n", Plan: tenantv1.Plan_PLAN_FREE}, nil
	}
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	rec, _ := h.graphql(t, tenantQuery, map[string]string{"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01"})

	if got := rec.Header().Get("X-Trace-Id"); got != traceID {
		t.Errorf("X-Trace-Id: got %q", got)
	}
	if got := h.fake.receivedTraceparent(); !strings.Contains(got, traceID) {
		t.Errorf("downstream traceparent: got %q", got)
	}
}
