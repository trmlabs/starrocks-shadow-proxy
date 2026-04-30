# Shadow Traffic Proxy for StarRocks

[![CI](https://github.com/trmlabs/starrocks-shadow-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/trmlabs/starrocks-shadow-proxy/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/trmlabs/starrocks-shadow-proxy)](go.mod)

A protocol-aware proxy that mirrors traffic from a primary database to a shadow database for performance comparison testing. Originally built for StarRocks (MySQL wire protocol); a Postgres / AlloyDB pgwire path is now available — see [docs/POSTGRES.md](docs/POSTGRES.md).

## Why

Upgrading or migrating StarRocks clusters is risky without knowing how the new cluster handles your actual production workload. This proxy lets you:

- **Compare performance** between StarRocks versions before cutting over
- **Validate configuration changes** on a shadow cluster with real traffic
- **Catch regressions** by monitoring P50/P90/P99 latency across both clusters
- **Analyze query patterns** via optional per-query logging to GCS

The proxy is transparent to clients -- they connect to it exactly as they would to StarRocks directly. Queries are forwarded synchronously to the primary cluster (zero added latency on the critical path) and mirrored asynchronously to the shadow cluster.

## Quick Start

```bash
# Start two local StarRocks clusters + the proxy + Prometheus + Grafana
docker compose -f docker-compose.local.yaml up --build

# Connect through the proxy (queries hit both clusters)
mysql -h 127.0.0.1 -P 3306 -u root -e "SELECT 1"

# View metrics
# Prometheus: http://localhost:9090/metrics
# Grafana:    http://localhost:3000 (admin/admin)
```

## Architecture

```
┌────────────────────────────────────────────────────────────────────────────────────────┐
│                                                                                        │
│  Client ════[TLS]════> Shadow Proxy ────[MySQL Packets]────> Primary FE Service       │
│                        (terminates TLS)       │              (sync, latency-sensitive) │
│                              │                │                                        │
│                              │     ┌──────────┴──────────┐                            │
│                              │     │ MySQL Packet Reader │                            │
│                              │     │ (buffered, handles  │                            │
│                              │     │  TCP fragmentation) │                            │
│                              │     └──────────┬──────────┘                            │
│                              │                │                                        │
│                              │                └───> Primary response ───> Client       │
│                              │                                                         │
│                              └────[MySQL Packets]────> Shadow Worker                   │
│                                  1:1 per client         (async mirroring)              │
│                                  Bounded queue          Complete packets only          │
│                                  (10K packets)          Protocol-aware drain           │
│                                                                                        │
│  Legend:                                                                               │
│    ════  Encrypted (TLS, proxy as server)                                              │
│    ────  MySQL protocol packets (proxy parses and forwards complete packets)           │
│                                                                                        │
└────────────────────────────────────────────────────────────────────────────────────────┘
```

### MySQL Packet-Based Forwarding

The proxy reads and forwards **complete MySQL packets** rather than raw TCP streams. This ensures:

- **No packet fragmentation**: Each shadow connection receives complete, valid MySQL packets
- **Accurate query counting**: Counts actual MySQL commands (`COM_QUERY`, `COM_STMT_EXECUTE`), not TCP operations
- **Protocol correctness**: Handles TCP fragmentation transparently via buffered reading
- **Multi-packet support**: Properly handles large queries (>16MB) that span multiple MySQL packets

**Supported MySQL commands tracked**:
| Command | Hex | Counted as Query |
|---------|-----|------------------|
| `COM_QUERY` | 0x03 | Yes |
| `COM_STMT_PREPARE` | 0x16 | Yes |
| `COM_STMT_EXECUTE` | 0x17 | Yes |
| `COM_PING` | 0x0E | No |
| `COM_QUIT` | 0x01 | No |
| `COM_INIT_DB` | 0x02 | No |
| Other admin commands | - | No |

### Shadow Worker (1:1 per Client)

Each client connection gets exactly **one** dedicated shadow worker:
- **1:1 Mapping**: One shadow connection per client connection (N clients = N shadow connections)
- **Bounded Queue**: Each worker has a bounded queue (default: 10,000 packets) for backpressure
- **Non-blocking Send**: Packets are queued asynchronously; if queue is full, packets are dropped (metric tracked)
- **SSL Support**: Optional TLS for shadow connections (proxy acts as TLS client via STARTTLS)
- **Complete Packets**: Shadow connection receives only complete MySQL packets (no fragmentation)
- **Protocol-Aware Drain**: Uses MySQL protocol parsing to read complete responses (no timeout-based detection)

#### Graceful Shutdown

When a client disconnects, the shadow worker performs a graceful drain to ensure all queued queries are processed:

1. **Stop accepting new queries**: Worker is marked as closed
2. **Signal worker to drain**: Worker receives a drain signal
3. **Process pending queries**: Worker processes all remaining packets in its queue
4. **Wait for completion**: System waits up to 60 seconds (configurable) for draining to complete
5. **Close connection**: Only after draining (or timeout) is the connection closed

This ensures accurate query counting—every query sent to primary is also sent to shadow, with no queries lost due to connection cleanup races.

## Features

- **MySQL Protocol Aware**: Parses MySQL packets to ensure complete commands are forwarded
- **TLS Termination**: Handles MySQL protocol SSL upgrade (STARTTLS, same as StarRocks FE)
- **Zero-latency mirroring**: Forwards queries to primary synchronously, mirrors to shadow asynchronously
- **1:1 Shadow Workers**: Each client gets a dedicated shadow connection with bounded queue (10K packets)
- **Protocol-Aware Response Parsing**: Reads complete MySQL responses without timeout-based detection
- **Accurate metrics**: Counts actual MySQL queries (COM_QUERY, COM_STMT_PREPARE, COM_STMT_EXECUTE)
- **Performance comparison**: Collects P50/P90/P95/P99 latency metrics for both clusters
- **Graceful Drain**: Ensures all queued queries are processed before shutdown
- **Transparent**: Clients connect the same way they would to StarRocks directly
- **Query Logging**: Optional per-query logging to GCS for BigQuery analysis (see below)

## How TLS Works

The proxy implements MySQL protocol SSL upgrade following the same pattern as StarRocks FE:

1. **Handshake**: Proxy reads handshake from primary FE, modifies it to advertise SSL support, sends to client
2. **SSL Request Detection**: Client sends capabilities with `CLIENT_SSL` flag (32-byte packet)
3. **TLS Upgrade**: Proxy performs TLS handshake with client using configured certificate
4. **Decrypted Proxying**: All subsequent traffic is decrypted, forwarded to backends over plain TCP

This allows clients to connect with `--ssl-mode=REQUIRED` while backend connections remain plain TCP.

## How MySQL Packet Handling Works

The proxy uses a **buffered MySQL packet reader** to ensure complete packets are forwarded:

1. **Buffered Reading**: TCP data is buffered until a complete MySQL packet is available
2. **Packet Parsing**: MySQL packet header (4 bytes) is parsed to determine payload length
3. **Complete Forwarding**: Only complete packets are forwarded to primary and shadow
4. **Command Detection**: The command byte is extracted to track query types accurately

**Why this matters for shadow mirroring**:

Without packet-aware handling, TCP fragmentation could cause partial packets on the shadow connection, leading to protocol errors. With packet-aware handling:
- Shadow worker receives complete, valid MySQL packets
- No protocol corruption
- Accurate query counting (counts actual `COM_QUERY` commands, not TCP reads)

```
Before (raw TCP):
  Client → [TCP seg 1] → Proxy → Shadow (partial packet - protocol error)
  Client → [TCP seg 2] → Proxy → Shadow (orphaned data - protocol error)

After (MySQL packets):
  Client → [TCP seg 1+2] → Proxy → [Complete Packet] → Shadow (valid query)
```

### Protocol-Aware Response Parsing

The shadow worker uses MySQL protocol parsing to read complete responses without timeout-based detection:

1. **Response Type Detection**: First packet's payload byte indicates response type:
   - `0x00`: OK packet (single packet, done)
   - `0xFF`: ERR packet (single packet, done)
   - `0xFE`: EOF packet (single packet, done)
   - Other: Result set (column count follows)

2. **Result Set Parsing**: For result sets:
   - Read column definition packets (count from first packet)
   - Read EOF packet after column definitions
   - Read row packets until EOF/OK marker

This eliminates the need for timeout-based response detection, ensuring accurate latency measurements.

## Metrics Exposed

### Core Metrics

| Metric | Description |
|--------|-------------|
| `shadow_proxy_query_duration_seconds` | Query latency histogram (labels: `target=primary\|shadow`) |
| `shadow_proxy_queries_total` | Total query count - only counts `COM_QUERY`, `COM_STMT_PREPARE`, `COM_STMT_EXECUTE` (labels: `target=primary\|shadow`) |
| `shadow_proxy_query_errors_total` | Total error count (labels: `target=primary\|shadow`) |
| `shadow_proxy_active_connections` | Current active connections |
| `shadow_proxy_connections_total` | Total connections accepted |

### Connection & Auth Metrics

| Metric | Description |
|--------|-------------|
| `shadow_proxy_connection_failures_total` | Connection failures (labels: `target=primary\|shadow`) |
| `shadow_proxy_auth_failures_total` | Authentication failures (labels: `target=shadow`) |
| `shadow_proxy_connections_with_shadow_total` | Connections successfully mirroring to shadow |
| `shadow_proxy_connections_without_shadow_total` | Connections that fell back to primary-only |

### Shadow I/O Metrics

| Metric | Description |
|--------|-------------|
| `shadow_proxy_read_timeouts_total` | Shadow read timeouts (no response within timeout) |
| `shadow_proxy_drain_timeouts_total` | Shadow drain timeouts (response not fully read) |
| `shadow_proxy_write_errors_total` | Shadow write errors |
| `shadow_proxy_bytes_total` | Bytes transferred (labels: `target`, `direction=sent\|received`) |

### TLS Metrics

| Metric | Description |
|--------|-------------|
| `shadow_proxy_tls_upgrades_total` | Successful TLS upgrades |
| `shadow_proxy_tls_failures_total` | TLS handshake failures |

### Health Metrics

| Metric | Description |
|--------|-------------|
| `shadow_proxy_primary_up` | Primary cluster reachable (1=up, 0=down) |
| `shadow_proxy_shadow_up` | Shadow cluster reachable (1=up, 0=down) |

### Worker Metrics

| Metric | Description |
|--------|-------------|
| `shadow_proxy_queue_drops_total` | Queries dropped due to full shadow queue |
| `shadow_proxy_queue_depth` | Current queue depth (sum across all workers) |

### MySQL Command Metrics

| Metric | Description |
|--------|-------------|
| `shadow_proxy_mysql_commands_total` | MySQL commands by type (labels: `target`, `command=COM_QUERY\|COM_STMT_EXECUTE\|...`) |
| `shadow_proxy_mysql_packets_total` | Total MySQL packets processed (labels: `target=primary\|shadow`) |

These metrics provide accurate query counting by tracking actual MySQL protocol commands rather than TCP operations.

## Query Logging (GCS → BigQuery)

The proxy can log per-query execution details to GCS for later analysis in BigQuery. This enables:

- **Per-query latency comparison**: Compare primary vs shadow execution time for each query
- **Query pattern analysis**: Find slow queries or queries with high primary/shadow variance
- **Historical analysis**: Query logs are retained for trend analysis

### How It Works

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                              Shadow Proxy                                     │
│                                                                               │
│  Query arrives ──> Generate UUID ──> Forward to Primary ──> Log primary time │
│                         │                                                     │
│                         └──> Queue for Shadow ──> Process async ──> Log shadow│
│                                                                    time       │
│                                     │                                         │
│                           [In-memory buffer]                                  │
│                           (channel, 10K entries)                              │
│                                     │                                         │
│                           Every 2 min OR 1000 entries                         │
│                                     │                                         │
│                                     ▼                                         │
│                           Flush to GCS as JSONL                               │
└──────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│  GCS Bucket (Hive-style partitioning)                                        │
│                                                                               │
│  gs://bucket/query-logs/                                                      │
│    └── year=2026/                                                             │
│        └── month=02/                                                          │
│            └── day=05/                                                        │
│                └── hour=14/                                                   │
│                    ├── 20260205_140000_a3f2b8c9.jsonl                        │
│                    ├── 20260205_140200_b4c5d6e7.jsonl                        │
│                    └── ...                                                    │
└──────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│  BigQuery External Table (with Hive partition pruning)                       │
│                                                                               │
│  SELECT * FROM query_logs                                                     │
│  WHERE year = '2026' AND month = '02'  -- Only scans relevant partitions!   │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Log Entry Schema

Each log entry contains:

| Field | Type | Description |
|-------|------|-------------|
| `ts` | string | ISO8601 timestamp (e.g., `2026-02-05T14:30:25.123Z`) |
| `query_id` | string | UUID for correlating primary ↔ shadow entries |
| `target` | string | `"primary"` or `"shadow"` |
| `command` | string | MySQL command (e.g., `COM_QUERY`, `COM_PING`) |
| `query_text` | string | Full SQL query text (only for `COM_QUERY`) |
| `duration_ms` | float | Execution time in milliseconds |
| `bytes_sent` | int | Bytes sent to target |
| `bytes_recv` | int | Bytes received from target |
| `success` | bool | Whether execution succeeded |
| `error` | string | Error message if failed |
| `client_addr` | string | Client IP address |

### Example Log Entries

```json
{"ts":"2026-02-05T14:30:25.123Z","query_id":"550e8400-e29b-41d4-a716-446655440000","target":"primary","command":"COM_QUERY","query_text":"SELECT * FROM users WHERE id = 123","duration_ms":45.2,"bytes_sent":156,"bytes_recv":4096,"success":true,"client_addr":"10.0.0.1:54321"}
{"ts":"2026-02-05T14:30:25.189Z","query_id":"550e8400-e29b-41d4-a716-446655440000","target":"shadow","command":"COM_QUERY","query_text":"SELECT * FROM users WHERE id = 123","duration_ms":67.8,"bytes_sent":156,"bytes_recv":4096,"success":true,"client_addr":"10.0.0.1:54321"}
```

Note: Both entries have the **same `query_id`** - this is how you correlate them in BigQuery.

### BigQuery Setup

1. **Create an external table with Hive partitioning**:

```sql
CREATE EXTERNAL TABLE `project.dataset.query_logs`
(
  ts TIMESTAMP,
  query_id STRING,
  target STRING,
  command STRING,
  query_text STRING,
  duration_ms FLOAT64,
  bytes_sent INT64,
  bytes_recv INT64,
  success BOOL,
  error STRING,
  client_addr STRING,
  query_hash STRING,
  filtered BOOL,
  filter_reason STRING
)
WITH PARTITION COLUMNS (
  year STRING,
  month STRING,
  day STRING,
  hour STRING
)
OPTIONS (
  format = 'JSON',
  uris = ['gs://your-bucket/query-logs/*'],
  hive_partition_uri_prefix = 'gs://your-bucket/query-logs/'
);
```

2. **Example queries**:

```sql
-- Find queries where shadow was significantly slower than primary
SELECT
  p.query_id,
  p.query_text,
  p.duration_ms AS primary_ms,
  s.duration_ms AS shadow_ms,
  s.duration_ms - p.duration_ms AS delta_ms
FROM `project.dataset.query_logs` p
JOIN `project.dataset.query_logs` s ON p.query_id = s.query_id
WHERE p.target = 'primary'
  AND s.target = 'shadow'
  AND p.year = '2026' AND p.month = '02' AND p.day = '05'  -- Partition pruning
  AND s.duration_ms > p.duration_ms * 2  -- Shadow took 2x longer
ORDER BY delta_ms DESC
LIMIT 100;

-- Average latency by query pattern
SELECT
  SUBSTR(query_text, 1, 100) AS query_preview,
  COUNT(*) / 2 AS execution_count,
  AVG(IF(target = 'primary', duration_ms, NULL)) AS avg_primary_ms,
  AVG(IF(target = 'shadow', duration_ms, NULL)) AS avg_shadow_ms
FROM `project.dataset.query_logs`
WHERE year = '2026' AND month = '02' AND day = '05'
GROUP BY query_preview
ORDER BY execution_count DESC;
```

### File Naming & Partitioning

- **Path format**: `gs://bucket/prefix/year=YYYY/month=MM/day=DD/hour=HH/YYYYMMDD_HHMMSS_xxxxxxxx.jsonl`
- **Hive-style partitioning**: BigQuery automatically prunes partitions when filtering by `year`, `month`, `day`, `hour`
- **File naming**: `{timestamp}_{random-8-chars}.jsonl` ensures uniqueness
- **No overwrites**: Each flush creates a new file; BigQuery scans all files in a partition

### Enabling Query Logging

Set the `QUERY_LOG_GCS_BUCKET` environment variable to enable:

```bash
# Enable query logging
QUERY_LOG_GCS_BUCKET=your-bucket-name

# Optional: customize settings
QUERY_LOG_GCS_PREFIX=query-logs
QUERY_LOG_FLUSH_INTERVAL_SECONDS=120
QUERY_LOG_BATCH_SIZE=1000
```

## Environment Variables

### Connection Settings

| Variable | Description | Default |
|----------|-------------|---------|
| `PRIMARY_HOST` | Primary StarRocks FE host | (required) |
| `PRIMARY_PORT` | Primary StarRocks FE port | `9030` |
| `PRIMARY_USER` | Primary cluster username | `root` |
| `PRIMARY_PASSWORD` | Primary cluster password | (required) |
| `SHADOW_HOST` | Shadow StarRocks FE host | (required) |
| `SHADOW_PORT` | Shadow StarRocks FE port | `9030` |
| `SHADOW_USER` | Shadow cluster username | `root` |
| `SHADOW_PASSWORD` | Shadow cluster password | (required) |
| `LISTEN_ADDR` | Proxy listen address | `:3306` |
| `METRICS_PORT` | Prometheus metrics port | `:9090` |

### TLS Settings (Proxy as Server - Client Connections)

| Variable | Description | Default |
|----------|-------------|---------|
| `TLS_ENABLED` | Enable TLS termination for client connections | `false` |
| `TLS_CERT_FILE` | Path to TLS certificate | `/certs/tls.crt` |
| `TLS_KEY_FILE` | Path to TLS private key | `/certs/tls.key` |

### Shadow TLS Settings (Proxy as Client - Shadow Connections)

| Variable | Description | Default |
|----------|-------------|---------|
| `SHADOW_TLS_ENABLED` | Enable TLS for shadow connections | `false` |
| `SHADOW_TLS_INSECURE` | Skip certificate verification (dev only) | `true` |

### Shadow Queue & Timeout Settings

| Variable | Description | Default |
|----------|-------------|---------|
| `SHADOW_QUEUE_SIZE` | Buffer size for shadow query queue per worker | `10000` |
| `SHADOW_READ_TIMEOUT_SECONDS` | Timeout waiting for shadow response | `30` |
| `SHADOW_DRAIN_TIMEOUT_MS` | Timeout for draining queue on client disconnect | `60000` |
| `SHADOW_RESPONSE_DRAIN_TIMEOUT_MS` | Per-read timeout when draining shadow response | `100` |

### Shadow Worker Settings

**Note**: Each client gets exactly one shadow connection with a bounded queue. The drain timeout (60 seconds by default) ensures queries are not lost during client disconnects.

### Query Logging Settings

| Variable | Description | Default |
|----------|-------------|---------|
| `QUERY_LOG_GCS_BUCKET` | GCS bucket for query logs (empty = disabled) | (disabled) |
| `QUERY_LOG_GCS_PREFIX` | Path prefix within bucket | `query-logs` |
| `QUERY_LOG_FLUSH_INTERVAL_SECONDS` | Flush interval (or when batch is full) | `120` (2 min) |
| `QUERY_LOG_BATCH_SIZE` | Max entries before forced flush | `1000` |
| `QUERY_LOG_BUFFER_SIZE` | In-memory channel buffer size | `10000` |

**Note**: Logs are flushed when either the batch size is reached OR the flush interval elapses, whichever comes first. This ensures low-latency logging during busy periods while still flushing during quiet periods.

### Shadow Query Filtering (Selective Mirroring)

By default, every `COM_QUERY` is mirrored to the shadow cluster. You can selectively control which queries get shadowed using **include** (allowlist) or **exclude** (blocklist) rules, optional **regex** patterns on full SQL text, and optional **random sampling**.

Only `COM_QUERY` commands are filtered. Other MySQL commands (`COM_INIT_DB`, `COM_STMT_PREPARE`, etc.) are always forwarded to keep the shadow session synchronized.

| Variable | Description | Default |
|----------|-------------|---------|
| `SHADOW_FILTER_MODE` | `include` or `exclude`. If unset, filtering is disabled (mirror all). | (disabled) |
| `SHADOW_FILTER_SQL_OPERATIONS` | Comma-separated SQL operation types to filter. StarRocks-aware: `SELECT`, `INSERT_OVERWRITE`, `SUBMIT_TASK`, `CREATE_MATERIALIZED_VIEW`, etc. For multi-statement queries (`SET CATALOG..; USE..; INSERT OVERWRITE..`), the primary (last non-preamble) operation is detected. | (empty) |
| `SHADOW_FILTER_PATTERNS` | Comma-separated Go regex patterns matched against the full query text. | (empty) |
| `SHADOW_SAMPLE_RATE` | Fraction of queries (that pass other filters) to actually shadow. `0.0`–`1.0`. | `1.0` |

**Filter logic:**
- **Include mode**: query must match ALL configured criteria (operations AND patterns). Within each, OR logic applies (match any one operation or any one pattern).
- **Exclude mode**: query is blocked if it matches ANY criterion (operations OR patterns).
- **Sampling** is applied last, after other filters pass.

**Examples:**

```bash
# Only shadow SELECT queries
SHADOW_FILTER_MODE=include
SHADOW_FILTER_SQL_OPERATIONS=SELECT

# Shadow everything except heavy ETL writes
SHADOW_FILTER_MODE=exclude
SHADOW_FILTER_SQL_OPERATIONS=INSERT_OVERWRITE,SUBMIT_TASK

# Only shadow queries touching analytics tables
SHADOW_FILTER_MODE=include
SHADOW_FILTER_PATTERNS=analytics\.,risk_indicators

# Exclude system catalog queries
SHADOW_FILTER_MODE=exclude
SHADOW_FILTER_PATTERNS=(?i)information_schema,(?i)__internal

# Shadow 10% of all queries (load testing)
SHADOW_SAMPLE_RATE=0.1

# Combine: only SELECT on analytics at 50% sample rate
SHADOW_FILTER_MODE=include
SHADOW_FILTER_SQL_OPERATIONS=SELECT
SHADOW_FILTER_PATTERNS=analytics\.
SHADOW_SAMPLE_RATE=0.5
```

**Metrics**: `shadow_proxy_shadow_filtered_total{reason="sql_operation|pattern|sampling"}` — counter of queries filtered from shadow, broken down by reason.

**GCS Logging**: When query logging is enabled, filtered queries are logged with `target=shadow`, `filtered=true`, and `filter_reason` so every primary entry has a corresponding shadow entry for BigQuery correlation analysis.

## Development

### Project Structure

The source is split into focused files within a single `package main`:

| File | Purpose |
|---|---|
| `main.go` | Entry point — config, health checks, HTTP server, signal handling |
| `config.go` | `Config` struct, env var loading (`loadConfig`, `getEnv`) |
| `metrics.go` | Prometheus metric declarations, `init()` registration, worker registry |
| `mysql_protocol.go` | MySQL constants, command helpers, `MySQLPacketReader`, packet utilities |
| `mysql_auth.go` | SSL/TLS detection, handshake modification, scramble extraction, native password |
| `shadow_worker.go` | `ShadowWorker` — per-client queue, async mirroring, graceful drain |
| `proxy.go` | `TCPProxy` — connection handling, SSL upgrade, auth, bidirectional proxying |
| `query_filter.go` | Selective query filtering — StarRocks-aware SQL operation detection, regex matching, sampling |
| `query_logger.go` | Async batched query logging to GCS (JSONL, Hive-partitioned) |

Test files mirror source files (e.g. `proxy.go` → `proxy_test.go`).

### Run Tests

```bash
# Unit tests (skips integration)
make test-unit

# All tests with race detection (prints summary at end)
make test

# With coverage report
make test-coverage
```

### Local Testing (without TLS)

```bash
docker compose -f docker-compose.local.yaml up --build

# Run basic connectivity tests
./test-local.sh

# Run filter integration tests (4 phases: baseline, operation, pattern, include)
./test-filter-integration.sh
# or:
make test-filter
```

### Local Testing (with TLS)

```bash
# Generate test certificates
./certs/generate-certs.sh

# Start with TLS
TLS_ENABLED=true docker compose -f docker-compose.local.yaml up --build

# Connect with SSL
mysql -h 127.0.0.1 -P 3306 -u root --ssl-mode=REQUIRED --ssl-ca=certs/ca.crt
```

### Build

```bash
# Build binary
make build

# Build Docker image (ARM64)
make docker-build-arm64

# Build and push to GHCR
make docker-release-arm64
```

## Deployment

### Docker

```bash
docker run -d \
  -e PRIMARY_HOST=primary-starrocks-fe \
  -e PRIMARY_PASSWORD=secret \
  -e SHADOW_HOST=shadow-starrocks-fe \
  -e SHADOW_PASSWORD=secret \
  -p 3306:3306 \
  -p 9090:9090 \
  ghcr.io/trmlabs/starrocks-shadow-proxy:latest
```

### Kubernetes

The `minikube/` directory contains a complete working example with:
- Primary and shadow StarRocks clusters (via the StarRocks Operator)
- Shadow proxy deployment with TLS
- Prometheus and Grafana monitoring

See [`minikube/README.md`](minikube/README.md) for setup instructions. Adapt the manifests for your production cluster.

**Typical production setup**:

1. **Create credentials secret**:
```bash
kubectl create secret generic shadow-proxy-credentials -n starrocks \
  --from-literal=PRIMARY_USER=root \
  --from-literal=PRIMARY_PASSWORD='<primary-password>' \
  --from-literal=SHADOW_USER=root \
  --from-literal=SHADOW_PASSWORD='<shadow-password>'
```

2. **Deploy the proxy** (adapt from `minikube/primary/shadow-proxy.yaml`)

3. **Connect through the proxy**:
```bash
LB_IP=$(kubectl get svc shadow-proxy-lb -n starrocks -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
mysql -h $LB_IP -P 9030 -u root --ssl-mode=REQUIRED
```

### TLS Certificates

For TLS termination, mount PEM-format certificate and key files and set:

```bash
TLS_ENABLED=true
TLS_CERT_FILE=/certs/tls.crt
TLS_KEY_FILE=/certs/tls.key
```

Generate self-signed certificates for testing with `./certs/generate-certs.sh`.

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
