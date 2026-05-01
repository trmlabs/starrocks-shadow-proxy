// metrics.go — Prometheus metric declarations, registration, and the worker
// registry used for accurate queue-depth tracking across shadow connections.
package main

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics for Prometheus
var (
	queryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "shadow_proxy_query_duration_seconds",
			Help:    "Query duration in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		},
		[]string{"target"},
	)
	queryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_queries_total",
			Help: "Total number of queries",
		},
		[]string{"target"},
	)
	queryErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_query_errors_total",
			Help: "Total number of query errors",
		},
		[]string{"target"},
	)
	activeConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shadow_proxy_active_connections",
			Help: "Number of active connections",
		},
	)
	tlsUpgrades = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_tls_upgrades_total",
			Help: "Total number of successful TLS upgrades",
		},
	)
	tlsFailures = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_tls_failures_total",
			Help: "Total number of TLS handshake failures",
		},
	)
	primaryUp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shadow_proxy_primary_up",
			Help: "Whether primary cluster is reachable (1=up, 0=down)",
		},
	)
	shadowUp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shadow_proxy_shadow_up",
			Help: "Whether shadow cluster is reachable (1=up, 0=down)",
		},
	)
	shadowQueueDrops = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_queue_drops_total",
			Help: "Total number of queries dropped due to full shadow queue",
		},
	)
	connectionFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_connection_failures_total",
			Help: "Total number of connection failures",
		},
		[]string{"target"},
	)
	authFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_auth_failures_total",
			Help: "Total number of authentication failures",
		},
		[]string{"target"},
	)
	connectionsWithoutShadow = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_connections_without_shadow_total",
			Help: "Total number of connections that fell back to primary-only mode",
		},
	)
	bytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_bytes_total",
			Help: "Total bytes transferred",
		},
		[]string{"target", "direction"}, // direction: sent, received
	)
	shadowReadTimeouts = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_read_timeouts_total",
			Help: "Total number of shadow read timeouts",
		},
	)
	shadowDrainTimeouts = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_drain_timeouts_total",
			Help: "Total number of shadow drain timeouts (response not fully read)",
		},
	)
	shadowWriteErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_write_errors_total",
			Help: "Total number of shadow write errors",
		},
	)
	totalConnections = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_connections_total",
			Help: "Total number of connections accepted",
		},
	)
	connectionsWithShadow = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_connections_with_shadow_total",
			Help: "Total number of connections successfully mirroring to shadow",
		},
	)
	// Shadow filter metrics
	shadowFilteredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_shadow_filtered_total",
			Help: "Total number of queries filtered from shadow mirroring",
		},
		[]string{"reason"}, // "sql_operation", "pattern", "sampling"
	)
	// MySQL command metrics for accurate query counting
	mysqlCommands = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_mysql_commands_total",
			Help: "Total number of MySQL commands by type and target",
		},
		[]string{"target", "command"},
	)
	mysqlPackets = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_mysql_packets_total",
			Help: "Total number of MySQL packets processed (including multi-packet sequences)",
		},
		[]string{"target"},
	)
	pgCommands = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_pg_commands_total",
			Help: "Total number of pgwire frontend messages by type and target",
		},
		[]string{"target", "command"},
	)
	pgPackets = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_pg_packets_total",
			Help: "Total number of pgwire frontend messages processed",
		},
		[]string{"target"},
	)
)

// Worker registry for accurate queue depth tracking
var (
	workerRegistryMu sync.RWMutex
	workerRegistry   = make(map[*ShadowWorker]struct{})
)

// getQueueDepth computes total queue depth by summing active worker queues.
// This eliminates race conditions that can occur with atomic counters when
// Send() and Close() execute concurrently.
func getQueueDepth() int64 {
	workerRegistryMu.RLock()
	defer workerRegistryMu.RUnlock()
	var total int64
	for w := range workerRegistry {
		total += int64(len(w.queue))
	}
	return total
}

func init() {
	prometheus.MustRegister(queryDuration)
	prometheus.MustRegister(queryTotal)
	prometheus.MustRegister(queryErrors)
	prometheus.MustRegister(activeConnections)
	prometheus.MustRegister(tlsUpgrades)
	prometheus.MustRegister(tlsFailures)
	prometheus.MustRegister(primaryUp)
	prometheus.MustRegister(shadowUp)
	prometheus.MustRegister(shadowQueueDrops)
	// Queue depth computed on-demand from worker registry to avoid race conditions
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "shadow_proxy_queue_depth",
			Help: "Current depth of the shadow queue (sum across all connections)",
		},
		func() float64 {
			return float64(getQueueDepth())
		},
	))
	prometheus.MustRegister(connectionFailures)
	prometheus.MustRegister(authFailures)
	prometheus.MustRegister(connectionsWithoutShadow)
	prometheus.MustRegister(bytesTotal)
	prometheus.MustRegister(shadowReadTimeouts)
	prometheus.MustRegister(shadowDrainTimeouts)
	prometheus.MustRegister(shadowWriteErrors)
	prometheus.MustRegister(totalConnections)
	prometheus.MustRegister(connectionsWithShadow)
	// Shadow filter metrics
	prometheus.MustRegister(shadowFilteredTotal)
	// MySQL command metrics
	prometheus.MustRegister(mysqlCommands)
	prometheus.MustRegister(mysqlPackets)
	// Postgres command metrics
	prometheus.MustRegister(pgCommands)
	prometheus.MustRegister(pgPackets)
}
