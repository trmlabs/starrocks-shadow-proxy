# Postgres / AlloyDB shadow proxy

This document covers the pgwire path. For the original StarRocks/MySQL path see the top-level [README](../README.md).

## What it does (PR #1 scope)

A pgwire-aware TCP proxy that:

- Accepts client connections on `:5432`.
- Opens a TCP connection to a single primary AlloyDB / Postgres backend.
- Forwards the startup/auth handshake transparently — no auth termination.
- Inspects each frontend message (Query, Parse, Bind, Execute, …), times the request/response round-trip, and emits per-query Prometheus metrics + an optional GCS log entry.
- Falls back to plain bidirectional `io.Copy` if the connection enters COPY mode (CopyInResponse / CopyOutResponse), sacrificing per-query timing for that connection.

**Not in PR #1 (deliberately):**

- No shadow mirroring. The `SHADOW_HOST` env var is read but unused. PR #2 wires up `ShadowWorker` to async-mirror frontend messages to a second backend.
- No TLS termination. Clients must connect with `sslmode=disable` (or `sslmode=prefer`, which falls back). When the client sends an `SSLRequest` and the server agrees ('S'), the proxy refuses the connection.
- No HA. Single replica only. Fine for staging at our QPS; needs multi-replica + connection-aware LB before customer traffic.
- No sampling. PR #2 adds connection-level sampling so we can throttle shadow load if needed.

## Environment variables

Selecting the protocol:

| Var | Default | Description |
|---|---|---|
| `PROTOCOL` | `mysql` | Set to `postgres` (or `pg` / `postgresql`) to use the pgwire path. |

Backend / listener (shared with the MySQL path; defaults differ when `PROTOCOL=postgres`):

| Var | Postgres default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:5432` | Where the proxy listens for client connections. |
| `PRIMARY_HOST` | _(required)_ | Primary backend host. |
| `PRIMARY_PORT` | `5432` | Primary backend port. |
| `PRIMARY_USER` | `root` | Currently informational — auth is forwarded transparently. |
| `PRIMARY_PASSWORD` | `""` | Same. Inject from Vault in production deployments. |
| `SHADOW_HOST` | `""` | Read but unused in PR #1. |
| `METRICS_PORT` | `:9090` | HTTP server for `/metrics`, `/health`, `/ready`. |
| `QUERY_LOG_GCS_BUCKET` | `""` | GCS bucket for JSONL query logs. Empty = disabled. |
| `QUERY_LOG_GCS_PREFIX` | `query-logs` | Path prefix within bucket. |
| `DEBUG_LOG` | `false` | Verbose per-connection traces. Off by default. |

Vault integration for secrets is a deployment concern — the proxy reads plain env vars. Wire `PRIMARY_PASSWORD` / `SHADOW_PASSWORD` from a Vault Agent sidecar or a Kubernetes secret synced from Vault.

## Local testing

```bash
./test-pg-local.sh           # full cycle, tears down on exit
./test-pg-local.sh --keep    # leaves the stack running
```

The script uses `docker-compose.pg.yaml`, which starts `postgres:15` (primary) and `postgres:18` (shadow, present for PR #2) plus the proxy. The proxy listens on `127.0.0.1:5432`; primary and shadow are exposed on `15432` / `25432` so you can `psql` either side directly for comparison.

The compose file uses vanilla `postgres:*` images so you don't need GCP credentials for local testing. To exercise AlloyDB-specific behavior (ColumnarEngine, IndexAdvisor, etc.), swap them for `gcr.io/alloydb-omni/alloydb-omni:*` tags. Wire-protocol behavior is identical between vanilla Postgres and AlloyDB Omni.

## Metrics added in this PR

| Metric | Type | Labels |
|---|---|---|
| `shadow_proxy_pg_commands_total` | counter | `target`, `command` (Query, Parse, Bind, Execute, …) |
| `shadow_proxy_pg_packets_total` | counter | `target` |

The existing `shadow_proxy_query_duration_seconds`, `shadow_proxy_queries_total`, `shadow_proxy_query_errors_total`, and `shadow_proxy_bytes_total{direction}` metrics are reused for the pgwire path with `target="primary"`.

## Design decisions worth re-litigating

- **Single-goroutine request/response loop**: simpler and gives accurate per-query timing, but breaks pgwire pipelining (Parse+Bind+Execute+Sync as a single batch is fine because we wait for ReadyForQuery; concurrent queries on one connection are not supported but pg drivers don't do that). If we hit a pipelining-heavy workload, revisit with a duplex parser.
- **Hand-rolled wire parser**: ~150 LOC, no `pgproto3` dependency. If the parser grows complex (e.g. when adding SCRAM auth termination), switch to `github.com/jackc/pgx/v5/pgproto3`.
- **COPY fallback**: we lose timing for COPY connections rather than implementing CopyData state-machine handling. cobalt does not use COPY, so this is acceptable for the upgrade-validation use case. Document and revisit if a COPY-heavy caller appears.
- **No auth termination**: keeps the proxy stateless and lets it work with any auth scheme the backend supports (cleartext, MD5, SCRAM, IAM tokens via `cloud-sql-proxy`-injected creds). The cost is that shadow auth in PR #2 needs to be configurable separately — likely shadow-side Vault creds.
