# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Experimental Postgres / AlloyDB transparent-forward proxy mode selected with `PROTOCOL=postgres`.
- Pgwire message decoding through `github.com/jackc/pgx/v5/pgproto3`.
- Postgres command and packet metrics.
- AlloyDB Omni local smoke test path via `make test-pg-local`.

## [1.0.0] - 2026-03-23

Initial open-source release.

### Features

- MySQL wire-protocol proxy with packet-level forwarding (no TCP fragmentation on shadow)
- 1:1 shadow worker per client connection with bounded queue (10K packets) and backpressure
- TLS termination for client connections (MySQL protocol SSL upgrade / STARTTLS)
- Optional TLS for shadow connections (proxy as TLS client)
- Protocol-aware response parsing for accurate latency measurement
- Graceful drain on client disconnect (configurable timeout, default 60s)
- Prometheus metrics: latency histograms, query counts, error rates, connection stats, queue depth
- Optional per-query logging to GCS as JSONL with Hive-style partitioning (for BigQuery or other analytics)
- Health and readiness endpoints (`/health`, `/ready`, `/status`)
- Docker Compose environments for local testing (with and without TLS)
- Minikube setup for Kubernetes testing with StarRocks Operator
- Grafana dashboard for monitoring
