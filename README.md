# ngumbah-gateway

The only entry point from the browser into Ngumbah. Serves GraphQL for business data and REST for health, and calls backend services over gRPC using [ngumbah-contracts](https://github.com/tahananazkaban/ngumbah-contracts) `v0.1.0`.

## Run locally

The gateway needs the tenant service. Start it with `make core-up` in `ngumbah-platform`, or run `ngumbah-tenant-svc` natively, then:

```sh
make env   # create .env from .env.example
make run   # serve on :8080 with live reload
```

Open the playground at <http://localhost:8080/playground> (development only), or:

```sh
curl -s localhost:8080/graphql -H 'Content-Type: application/json' \
  -d '{"query":"{ tenant(id: \"019a0000-0000-7000-8000-000000000001\") { id name plan } }"}'
```

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /graphql` (`GET` for queries) | GraphQL API |
| `GET /playground` | GraphQL playground, development only |
| `GET /livez` | 200 while the process runs |
| `GET /readyz` | 200 when every dependency is `SERVING`, otherwise 503; lists dependencies |

## GraphQL

Schema: `graph/schema.graphqls`.

```graphql
type Query { tenant(id: ID!): Tenant }
type Tenant { id: ID!, name: String!, plan: Plan! }
enum Plan { FREE PRO }
```

Errors carry a stable `extensions.code`:

| Code | When | Client action |
|---|---|---|
| `NOT_FOUND` | No tenant with that ID (`tenant` is `null`) | Show "not found" |
| `BAD_USER_INPUT` | ID is not a UUID | Fix the input |
| `SERVICE_UNAVAILABLE` | Tenant service unreachable or slower than 3 s | Offer a retry |
| `INTERNAL` | Anything else | Show a generic error |

Messages never include internal details; those are logged with the request's trace ID.

## Limits and security

- CORS allows only origins in `GATEWAY_CORS_ORIGINS` (`*` is rejected).
- Request bodies above 1 MB get 413; query complexity is capped at 100.
- Introspection and the playground are enabled only when `GATEWAY_ENV=development`. The default is `production`.

## Tracing and logs

Each request starts a W3C trace or continues the caller's `traceparent`. The trace is propagated to backend services over gRPC, and the trace ID is returned in the `X-Trace-Id` response header. Logs are JSON, one line per request with method, path, status, duration, GraphQL operation name, `trace_id` and `span_id`.

## Configuration

| Variable | Default |
|---|---|
| `GATEWAY_HTTP_ADDR` | `:8080` |
| `GATEWAY_TENANT_SVC_ADDR` | `localhost:50051` |
| `GATEWAY_CORS_ORIGINS` | `http://localhost:5173` (comma-separated) |
| `GATEWAY_ENV` | `production` (`development` enables playground and introspection) |
| `GATEWAY_LOG_LEVEL` | `info` |
| `OTEL_TRACES_EXPORTER` | `none` |

## Commands

| Command | What it does |
|---|---|
| `make build` / `make run` | Build `bin/gateway` / serve with live reload |
| `make test` | Run tests (no external services needed) |
| `make lint` | golangci-lint (in Docker) |
| `make generate` / `make generate-check` | Regenerate gRPC and GraphQL code / fail if committed code is stale |
| `make docker-build` | Build the container image |

The binary has two subcommands: `serve` (default) and `health` (probes `/readyz`, used by the container).
