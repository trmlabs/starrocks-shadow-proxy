package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// Query logger metrics for Prometheus observability
var (
	queryLoggerBufferCapacity = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shadow_proxy_query_logger_buffer_capacity",
			Help: "Total capacity of the query logger buffer",
		},
	)
	queryLoggerEntriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_query_logger_entries_total",
			Help: "Total number of log entries received by the query logger",
		},
		[]string{"status"}, // "queued" or "dropped"
	)
	queryLoggerFlushesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shadow_proxy_query_logger_flushes_total",
			Help: "Total number of flush operations",
		},
		[]string{"status"}, // "success" or "error"
	)
	queryLoggerFlushDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "shadow_proxy_query_logger_flush_duration_seconds",
			Help:    "Duration of flush operations to GCS",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
	)
	queryLoggerEntriesPerFlush = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "shadow_proxy_query_logger_entries_per_flush",
			Help:    "Number of entries written per flush operation",
			Buckets: []float64{1, 10, 50, 100, 250, 500, 1000, 2000},
		},
	)
	queryLoggerBytesWritten = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_query_logger_bytes_written_total",
			Help: "Total bytes written to GCS",
		},
	)
	queryLoggerLastFlushTimestamp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shadow_proxy_query_logger_last_flush_timestamp_seconds",
			Help: "Unix timestamp of the last successful flush",
		},
	)
	queryLoggerFallbackWrites = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shadow_proxy_query_logger_fallback_writes_total",
			Help: "Total number of entries written to fallback log (stdout) due to GCS errors",
		},
	)
)

// queryLoggerMetricsRegistered tracks if metrics have been registered
var queryLoggerMetricsRegistered bool
var queryLoggerMetricsMu sync.Mutex

// registerQueryLoggerMetrics registers Prometheus metrics (safe to call multiple times)
func registerQueryLoggerMetrics() {
	queryLoggerMetricsMu.Lock()
	defer queryLoggerMetricsMu.Unlock()

	if queryLoggerMetricsRegistered {
		return
	}

	prometheus.MustRegister(queryLoggerBufferCapacity)
	prometheus.MustRegister(queryLoggerEntriesTotal)
	prometheus.MustRegister(queryLoggerFlushesTotal)
	prometheus.MustRegister(queryLoggerFlushDuration)
	prometheus.MustRegister(queryLoggerEntriesPerFlush)
	prometheus.MustRegister(queryLoggerBytesWritten)
	prometheus.MustRegister(queryLoggerLastFlushTimestamp)
	prometheus.MustRegister(queryLoggerFallbackWrites)

	queryLoggerMetricsRegistered = true
}

// QueryLogEntry represents a single query execution record for BigQuery analysis
type QueryLogEntry struct {
	Timestamp  string  `json:"ts"`                    // ISO8601 timestamp
	QueryID    string  `json:"query_id"`              // UUID for primary/shadow correlation
	Target     string  `json:"target"`                // "primary" or "shadow"
	Command    string  `json:"command"`               // COM_QUERY, COM_PING, etc.
	QueryText  string  `json:"query_text,omitempty"`  // Full query text (only for COM_QUERY)
	QueryHash  string  `json:"query_hash,omitempty"`  // MD5 hash of query text for joining with FE profiles
	DurationMs float64 `json:"duration_ms"`           // Execution time in milliseconds
	BytesSent  int64   `json:"bytes_sent"`            // Bytes sent to target
	BytesRecv  int64   `json:"bytes_recv"`            // Bytes received from target
	Success    bool    `json:"success"`               // Whether execution succeeded
	Error      string  `json:"error,omitempty"`       // Error message if failed
	ClientAddr string  `json:"client_addr,omitempty"` // Client IP address
}

// QueryRequest contains the packet plus metadata for logging correlation.
// This is passed to the shadow worker so both primary and shadow can log
// with the same QueryID for correlation.
type QueryRequest struct {
	ID         string    // UUID for log correlation
	Packet     []byte    // Raw MySQL packet to send
	QueryText  string    // Extracted query text (empty for non-query commands)
	QueryHash  string    // MD5 hash of query text for joining with FE profiles
	Command    string    // MySQL command name (COM_QUERY, COM_PING, etc.)
	ClientAddr string    // Client address for context
	ReceivedAt time.Time // When packet was received from client
}

// NewQueryRequest creates a QueryRequest from a MySQL packet with correlation metadata
func NewQueryRequest(packet []byte, clientAddr string) QueryRequest {
	cmd, ok := getMySQLCommand(packet)
	cmdName := "UNKNOWN"
	if ok {
		cmdName = getMySQLCommandName(cmd)
	}

	var queryText string
	var queryHash string
	if cmd == comQuery && len(packet) > 5 {
		queryText = string(packet[5:])
		queryHash = fmt.Sprintf("%x", md5.Sum([]byte(queryText)))
	}

	return QueryRequest{
		ID:         uuid.New().String(),
		Packet:     packet,
		QueryText:  queryText,
		QueryHash:  queryHash,
		Command:    cmdName,
		ClientAddr: clientAddr,
		ReceivedAt: time.Now(),
	}
}

// QueryLoggerConfig holds configuration for the query logger
type QueryLoggerConfig struct {
	GCSBucket     string        // GCS bucket name (without gs://)
	GCSPrefix     string        // Path prefix within bucket
	FlushInterval time.Duration // How often to flush (e.g., 60s)
	BatchSize     int           // Max entries before forced flush
	BufferSize    int           // Channel buffer size
}

// QueryLogger handles async batching and writing query logs to GCS
type QueryLogger struct {
	entries       chan QueryLogEntry
	bucket        string
	prefix        string
	flushInterval time.Duration
	batchSize     int
	bufferSize    int // Store for metrics

	client *storage.Client
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// For buffer depth metric (registered once per logger instance)
	bufferDepthGauge prometheus.GaugeFunc
}

// NewQueryLogger creates a new query logger that writes to GCS
func NewQueryLogger(cfg QueryLoggerConfig) (*QueryLogger, error) {
	// Register metrics (idempotent)
	registerQueryLoggerMetrics()

	ctx, cancel := context.WithCancel(context.Background())

	client, err := storage.NewClient(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create GCS client: %w", err)
	}

	// Verify bucket access
	bucket := client.Bucket(cfg.GCSBucket)
	if _, err := bucket.Attrs(ctx); err != nil {
		cancel()
		client.Close()
		return nil, fmt.Errorf("failed to access GCS bucket %s: %w", cfg.GCSBucket, err)
	}

	entries := make(chan QueryLogEntry, cfg.BufferSize)

	ql := &QueryLogger{
		entries:       entries,
		bucket:        cfg.GCSBucket,
		prefix:        cfg.GCSPrefix,
		flushInterval: cfg.FlushInterval,
		batchSize:     cfg.BatchSize,
		bufferSize:    cfg.BufferSize,
		client:        client,
		ctx:           ctx,
		cancel:        cancel,
	}

	// Set buffer capacity metric
	queryLoggerBufferCapacity.Set(float64(cfg.BufferSize))

	// Register buffer depth gauge (reads current channel length).
	// Use Register (not MustRegister) to avoid panics if a previous logger
	// instance was not fully cleaned up (e.g., Close() failed or was skipped).
	ql.bufferDepthGauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "shadow_proxy_query_logger_buffer_depth",
			Help: "Current number of entries in the query logger buffer",
		},
		func() float64 {
			return float64(len(ql.entries))
		},
	)
	if err := prometheus.Register(ql.bufferDepthGauge); err != nil {
		log.Printf("QueryLogger: warning: failed to register buffer_depth gauge (may already exist): %v", err)
	}

	return ql, nil
}

// Start begins the background flush goroutine
func (ql *QueryLogger) Start() {
	ql.wg.Add(1)
	go ql.flushLoop()
	log.Printf("QueryLogger: started (bucket=%s, prefix=%s, flush_interval=%v, batch_size=%d)",
		ql.bucket, ql.prefix, ql.flushInterval, ql.batchSize)
}

// Log queues a log entry (non-blocking, drops if buffer full)
func (ql *QueryLogger) Log(entry QueryLogEntry) {
	select {
	case ql.entries <- entry:
		// Successfully queued
		queryLoggerEntriesTotal.WithLabelValues("queued").Inc()
	default:
		// Buffer full, drop entry and log warning
		queryLoggerEntriesTotal.WithLabelValues("dropped").Inc()
		log.Printf("QueryLogger: buffer full, dropping entry for query_id=%s target=%s",
			entry.QueryID, entry.Target)
	}
}

// Close gracefully shuts down the logger, flushing remaining entries
func (ql *QueryLogger) Close() error {
	log.Printf("QueryLogger: shutting down...")
	ql.cancel()
	ql.wg.Wait()

	// Unregister buffer depth gauge to allow re-registration if new logger is created
	if ql.bufferDepthGauge != nil {
		prometheus.Unregister(ql.bufferDepthGauge)
	}

	log.Printf("QueryLogger: shutdown complete")
	return ql.client.Close()
}

// flushLoop runs in background, batching and writing to GCS
func (ql *QueryLogger) flushLoop() {
	defer ql.wg.Done()

	ticker := time.NewTicker(ql.flushInterval)
	defer ticker.Stop()

	batch := make([]QueryLogEntry, 0, ql.batchSize)

	for {
		select {
		case entry, ok := <-ql.entries:
			if !ok {
				// Channel closed, flush and exit
				if len(batch) > 0 {
					ql.flush(batch)
				}
				return
			}
			batch = append(batch, entry)
			if len(batch) >= ql.batchSize {
				ql.flush(batch)
				batch = batch[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				ql.flush(batch)
				batch = batch[:0]
			}

		case <-ql.ctx.Done():
			// Shutdown requested, drain channel and flush
			for {
				select {
				case entry := <-ql.entries:
					batch = append(batch, entry)
				default:
					goto done
				}
			}
		done:
			if len(batch) > 0 {
				ql.flush(batch)
			}
			return
		}
	}
}

// flush writes a batch of entries to GCS as JSONL (newline-delimited JSON)
func (ql *QueryLogger) flush(batch []QueryLogEntry) {
	if len(batch) == 0 {
		return
	}

	flushStart := time.Now()

	// Generate object path with Hive-style partitioning for BigQuery partition pruning
	// Format: prefix/year=YYYY/month=MM/day=DD/hour=HH/timestamp_uuid.jsonl
	// This allows BigQuery to efficiently prune partitions when filtering by time
	now := time.Now().UTC()
	objectPath := fmt.Sprintf("%s/year=%s/month=%s/day=%s/hour=%s/%s_%s.jsonl",
		ql.prefix,
		now.Format("2006"),
		now.Format("01"),
		now.Format("02"),
		now.Format("15"),
		now.Format("20060102_150405"), // Timestamp prefix for ordering
		uuid.New().String()[:8],       // Short UUID suffix for uniqueness
	)

	// Convert batch to JSONL
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for _, entry := range batch {
		if err := encoder.Encode(entry); err != nil {
			log.Printf("QueryLogger: failed to encode entry: %v", err)
			continue
		}
	}
	bytesToWrite := int64(buf.Len())

	// Write to GCS with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj := ql.client.Bucket(ql.bucket).Object(objectPath)
	writer := obj.NewWriter(ctx)
	writer.ContentType = "application/x-ndjson"

	if _, err := io.Copy(writer, &buf); err != nil {
		log.Printf("QueryLogger: failed to write to GCS: %v (path=%s, entries=%d)",
			err, objectPath, len(batch))
		writer.Close()
		queryLoggerFlushesTotal.WithLabelValues("error").Inc()
		queryLoggerFlushDuration.Observe(time.Since(flushStart).Seconds())
		ql.fallbackLog(batch)
		return
	}

	if err := writer.Close(); err != nil {
		log.Printf("QueryLogger: failed to close GCS writer: %v (path=%s)", err, objectPath)
		queryLoggerFlushesTotal.WithLabelValues("error").Inc()
		queryLoggerFlushDuration.Observe(time.Since(flushStart).Seconds())
		ql.fallbackLog(batch)
		return
	}

	// Record successful flush metrics
	flushDuration := time.Since(flushStart)
	queryLoggerFlushesTotal.WithLabelValues("success").Inc()
	queryLoggerFlushDuration.Observe(flushDuration.Seconds())
	queryLoggerEntriesPerFlush.Observe(float64(len(batch)))
	queryLoggerBytesWritten.Add(float64(bytesToWrite))
	queryLoggerLastFlushTimestamp.Set(float64(time.Now().Unix()))

	log.Printf("QueryLogger: flushed %d entries (%d bytes) to gs://%s/%s in %v",
		len(batch), bytesToWrite, ql.bucket, objectPath, flushDuration)
}

// fallbackLog writes to stdout as JSON if GCS fails (so logs aren't lost)
func (ql *QueryLogger) fallbackLog(batch []QueryLogEntry) {
	queryLoggerFallbackWrites.Add(float64(len(batch)))
	for _, entry := range batch {
		data, err := json.Marshal(entry)
		if err != nil {
			log.Printf("QueryLogger[FALLBACK]: failed to marshal entry: %v", err)
			continue
		}
		log.Printf("QueryLogger[FALLBACK]: %s", string(data))
	}
}

// errorString safely converts an error to string, returning empty string for nil
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
