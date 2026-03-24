# Usage Patterns

This guide describes common patterns for analyzing query logs produced by the shadow proxy.

## Analyzing Query Logs with BigQuery

The proxy writes JSONL query logs to GCS with Hive-style partitioning. BigQuery can query these directly as an external table with automatic partition pruning.

### 1. Create an External Table

```sql
CREATE EXTERNAL TABLE `your_project.your_dataset.shadow_proxy_query_logs`
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
  query_hash STRING
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

### 2. Find Queries Where Shadow Is Slower

Join primary and shadow log entries on `query_id` to compare latency:

```sql
SELECT
  p.query_id,
  SUBSTR(p.query_text, 1, 120) AS query_preview,
  p.duration_ms AS primary_ms,
  s.duration_ms AS shadow_ms,
  ROUND(s.duration_ms / NULLIF(p.duration_ms, 0), 2) AS slowdown_factor
FROM `your_project.your_dataset.shadow_proxy_query_logs` p
JOIN `your_project.your_dataset.shadow_proxy_query_logs` s
  ON p.query_id = s.query_id
WHERE p.target = 'primary'
  AND s.target = 'shadow'
  AND p.year = '2026' AND p.month = '03'
  AND s.duration_ms > p.duration_ms * 2
ORDER BY s.duration_ms - p.duration_ms DESC
LIMIT 100;
```

### 3. Aggregate Latency by Query Pattern

```sql
SELECT
  SUBSTR(query_text, 1, 100) AS query_preview,
  COUNT(*) / 2 AS execution_count,
  ROUND(AVG(IF(target = 'primary', duration_ms, NULL)), 1) AS avg_primary_ms,
  ROUND(AVG(IF(target = 'shadow', duration_ms, NULL)), 1) AS avg_shadow_ms,
  ROUND(APPROX_QUANTILES(IF(target = 'primary', duration_ms, NULL), 100)[OFFSET(95)], 1) AS p95_primary_ms,
  ROUND(APPROX_QUANTILES(IF(target = 'shadow', duration_ms, NULL), 100)[OFFSET(95)], 1) AS p95_shadow_ms
FROM `your_project.your_dataset.shadow_proxy_query_logs`
WHERE year = '2026' AND month = '03'
  AND command = 'COM_QUERY'
GROUP BY query_preview
HAVING execution_count > 10
ORDER BY execution_count DESC;
```

### 4. Error Rate Comparison

```sql
SELECT
  target,
  COUNTIF(success) AS successes,
  COUNTIF(NOT success) AS failures,
  ROUND(COUNTIF(NOT success) / COUNT(*) * 100, 2) AS error_rate_pct
FROM `your_project.your_dataset.shadow_proxy_query_logs`
WHERE year = '2026' AND month = '03' AND day = '15'
GROUP BY target;
```

## Using StarRocks Execution Profiles

For queries that show significant latency differences, StarRocks execution profiles provide deeper insight.

### Fetch Profiles

```sql
-- On primary cluster
SHOW PROFILELIST LIMIT 100;
ANALYZE PROFILE <query_id>;

-- On shadow cluster (same queries, compare plans)
SHOW PROFILELIST LIMIT 100;
ANALYZE PROFILE <query_id>;
```

### What to Look For

- **Scan ranges**: Shadow may have different tablet distribution or partition pruning behavior
- **Exchange nodes**: Different parallelism or data shuffle strategies
- **Join order**: Optimizer may choose different join plans on the shadow cluster
- **Memory usage**: Peak memory per operator can reveal configuration differences

## Alternative Analytics Backends

The JSONL log format is not BigQuery-specific. You can load the same files into:

- **DuckDB**: `SELECT * FROM read_json_auto('query-logs/**/*.jsonl')` for local analysis
- **ClickHouse**: Create a table with the same schema and use `s3()` table function
- **Apache Spark**: Read with `spark.read.json("gs://bucket/query-logs/")` using Hive partition discovery
- **Pandas**: `pd.read_json("file.jsonl", lines=True)` for small datasets

## Prometheus Metrics

The proxy exposes Prometheus metrics at `/metrics` (default port 9090). Key metrics for comparison:

- `shadow_proxy_query_duration_seconds{target="primary|shadow"}` -- latency histograms
- `shadow_proxy_queries_total{target="primary|shadow"}` -- query counts
- `shadow_proxy_query_errors_total{target="primary|shadow"}` -- error counts

A sample Grafana dashboard is included in `monitoring/grafana/dashboards/`.
