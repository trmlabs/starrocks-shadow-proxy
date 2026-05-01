# Postgres / AlloyDB Proxy Mode

This mode adds a Postgres pgwire transparent-forward proxy alongside the existing StarRocks/MySQL path.

## Scope

Set `PROTOCOL=postgres` to run the pgwire path. In this version, the proxy:

- Listens on `:5432` by default.
- Forwards startup and authentication transparently to one primary Postgres or AlloyDB backend.
- Uses `github.com/jackc/pgx/v5/pgproto3` to decode pgwire messages for observability while forwarding the original bytes.
- Emits query duration, query count, bytes, connection, and Postgres command metrics.
- Optionally writes query logs through the existing GCS `QueryLogger`.

This version intentionally does not mirror to a shadow Postgres backend yet. Shadow mirroring needs separate shadow authentication, session state replay, prepared statement lifecycle handling, and desync recovery.

## TLS and GSS

TLS termination is out of scope for this version. The proxy declines Postgres `SSLRequest` and `GSSENCRequest` probes with `N`, so clients should use:

```bash
sslmode=disable gssencmode=disable
```

## Local AlloyDB Omni Smoke Test

The local smoke path uses Docker with `google/alloydbomni:latest`, which Google documents as the AlloyDB Omni container image for current releases. The user-requested reference is the AlloyDB Omni 18.1.0 container overview: https://docs.cloud.google.com/alloydb/omni/containers/18.1.0/docs/overview

Run:

```bash
make test-pg-local
```

This starts AlloyDB Omni plus the proxy, then runs a `pgx` integration test through the proxy using extended protocol queries and batches.
For manual local access, the proxy is published on `127.0.0.1:55432` to avoid colliding with a local Postgres server.

## Important Metrics

- `shadow_proxy_pg_commands_total{target,command}`: frontend pgwire messages observed by command type.
- `shadow_proxy_pg_packets_total{target}`: frontend pgwire messages processed.
- `shadow_proxy_query_duration_seconds{target="primary"}`: completed query cycle latency, ending on backend `ReadyForQuery`.
- `shadow_proxy_queries_total{target="primary"}`: countable query-bearing frontend messages.

## Known Limits

- Primary-only forwarding.
- No TLS termination.
- No HA or load balancing.
- No query filtering or sampling.
- COPY data is forwarded by the duplex loop, but detailed per-COPY timing is not modeled separately.
