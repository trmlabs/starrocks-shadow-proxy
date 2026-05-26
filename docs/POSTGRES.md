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
- No HA. Single replica only. Fine for staging at our QPS; needs multi-replica + connection-aware LB before customer traffic.
- No sampling. PR #2 adds connection-level sampling so we can throttle shadow load if needed.

**Added in the TLS follow-up (commit 459946d):**

- Listener-side TLS termination (gated on `TLS_ENABLED=true`) — the proxy
  replies `'S'` to client `SSLRequest` and wraps the connection in
  `tls.Server` using the configured cert + key.
- Backend-side TLS initiation (gated on `PRIMARY_TLS_ENABLED=true`) — before
  forwarding any pgwire framing, the proxy sends its own `SSLRequest` to the
  primary and wraps the connection in `tls.Client`. Required for AlloyDB,
  whose `pg_hba` refuses plaintext.

The two hops are independent. A common AlloyDB production posture is
`TLS_ENABLED=false` (pgbouncer → proxy stays plaintext within the VPC) and
`PRIMARY_TLS_ENABLED=true` (proxy → AlloyDB is TLS). Local development
against `docker-compose.pg-tls.yaml` sets both to true to mirror a future
cert-fronted deploy.

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

TLS:

| Var | Default | Description |
|---|---|---|
| `TLS_ENABLED` | `false` | Listener-side TLS termination. When true, the proxy presents `TLS_CERT_FILE` to clients. |
| `TLS_CERT_FILE` | `/certs/tls.crt` | Server cert for listener TLS. |
| `TLS_KEY_FILE` | `/certs/tls.key` | Server key for listener TLS. Must be 0600. |
| `PRIMARY_TLS_ENABLED` | `false` | Backend-side TLS initiation. Required against AlloyDB. |
| `PRIMARY_TLS_CA_FILE` | `""` | PEM bundle for verifying the backend cert. Empty = system roots. |
| `PRIMARY_TLS_INSECURE_SKIP_VERIFY` | `false` | Dev only (self-signed). Must be `false` in production. |
| `SHADOW_TLS_ENABLED` | `false` | TLS for the shadow hop (proxy → shadow backend). |
| `SHADOW_TLS_INSECURE` | `false` | Skip cert verification on the shadow hop. Dev/staging only — startup logs a `WARNING` when set to `true`. |

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

## Shadow query filtering and sampling

The shadow worker honors the same `SHADOW_FILTER_MODE` / `SHADOW_FILTER_SQL_OPERATIONS` / `SHADOW_FILTER_PATTERNS` / `SHADOW_SAMPLE_RATE` env vars as the MySQL path. Filtering only applies to SQL-carrying frames (Query, Parse); other frames (Bind, Execute, Sync, etc.) always pass through so the shadow's prepared-statement lifecycle stays in sync with the primary.

| Var | Default | Description |
|---|---|---|
| `SHADOW_FILTER_MODE` | `""` | `include` mirrors only matching queries; `exclude` mirrors everything except matches. Empty disables filtering. |
| `SHADOW_FILTER_SQL_OPERATIONS` | _(empty)_ | Comma-separated operation classes (`SELECT`, `INSERT`, …) — pg's `Query`/`Parse` are matched by their statement text. |
| `SHADOW_FILTER_PATTERNS` | _(empty)_ | Comma-separated regex patterns matched against the full query text. |
| `SHADOW_SAMPLE_RATE` | `1.0` | Fraction of SQL-carrying frames to mirror after filter checks. `0.0` skips all; `1.0` mirrors all. |

Filtered frames are counted under `shadow_proxy_shadow_filtered_total{reason="sql_operation"|"pattern"|"sampling"}` and (when GCS query logging is enabled) emitted as `target="shadow",filtered=true` log rows so the BigQuery primary↔shadow join stays one-to-one.

## Performance — TLS read path dominates CPU at high concurrency

The cross-cluster bench (3-replica proxy at 2 vCPU each, `pgbench -M extended` sweep 1/8/32/64/128 concurrent clients, both hops TLS-terminated against staging AlloyDB read pools) shows proxy overhead growing with concurrency: roughly **+2 ms at 1c, +20 ms at 32c, +135 ms at 128c** (b−a in the PR write-up). The whole latency distribution shifts together — p50, p90, and p99 grow at the same rate, so it's not a long-tail effect.

A 30 s CPU profile during the 128c run (`/debug/pprof/profile?seconds=30`) attributes ~62% of CPU to the TLS read path:

```
flat  flat%   sum%        cum   cum%
0.75s 20.72% 20.72%      0.75s 20.72%  internal/runtime/syscall/linux.Syscall6
0.70s 19.34% 40.06%      1.48s 40.88%  io.ReadAtLeast
0.62s 17.13% 57.18%      0.78s 21.55%  crypto/tls.(*Conn).Read
0.29s  8.01% 65.19%      1.01s 27.90%  crypto/tls.(*Conn).Write
0.18s  4.97% 70.17%      1.46s 40.33%  main.(*PgProxy).forwardResponseUntilReady
```

This is **not** a hot spot we can micro-optimize away in our code:

- **Not query hashing.** `crypto/md5.Sum` is well below 1% of CPU; the per-message hashing the proxy does for log correlation is in the noise.
- **Not TLS handshakes.** `tls.handshakeContext` is 0.29 s cum (8%). Handshakes amortize across the lifetime of a long-lived client connection — at 128 concurrent connections held open by `pgbench`, there's effectively one handshake per connection over the whole 30 s window.
- **Not the shadow worker.** `PgShadowWorker.processFrame` is 1.7% cumulative; mirroring overhead is sub-noise on the primary path (see PR #9's c−b column).

It **is** fundamental to Go's `crypto/tls` read path under many concurrent connections: epoll syscalls + the TLS record-layer decrypt loop in `(*Conn).Read`. Real options if this becomes a bottleneck:

- Larger TLS read buffers, or batched reads across goroutines.
- Kernel TLS (`KTLS`) — Linux can offload the record layer to the kernel and remove the userspace `crypto/tls.(*Conn).Read` hop entirely.
- An entirely different transport (`quic-go` etc.) once the workload is settled.

None of these are worth doing speculatively — record the finding here, run the longer-duration replay (≥30 min/cell) before any production-bound decision.
