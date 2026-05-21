// config.go — Proxy configuration struct, environment variable loading,
// and helpers for reading typed values from env vars with defaults.
package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the proxy configuration
type Config struct {
	// Protocol selects the wire protocol the proxy speaks.
	// Supported values: "mysql" (default, StarRocks/MySQL-compatible),
	// "postgres" (alias: "pg", "postgresql") for AlloyDB / Postgres.
	Protocol        string
	ListenAddr      string
	PrimaryHost     string
	PrimaryPort     string
	PrimaryUser     string
	PrimaryPassword string
	ShadowHost      string
	ShadowPort      string
	ShadowUser      string
	ShadowPassword  string
	MetricsPort     string
	// TLS configuration for client connections (proxy as server).
	// When the proxy is the pgwire endpoint and TLSEnabled is true, the proxy
	// will respond 'S' to a client's SSLRequest and upgrade the connection
	// via crypto/tls using the configured cert+key. When TLSEnabled is false
	// (transparent-forward mode), the proxy refuses to terminate TLS and
	// requires clients to use sslmode=disable.
	TLSEnabled  bool
	TLSCertFile string
	TLSKeyFile  string
	// TLS configuration for the primary backend connection (proxy as client).
	// PrimaryTLSEnabled causes the proxy to initiate a pgwire SSLRequest
	// against the primary and upgrade the backend connection to TLS before
	// forwarding the client's StartupMessage. AlloyDB's default pg_hba
	// requires TLS for non-loopback connections, so this must be true for
	// AlloyDB targets.
	PrimaryTLSEnabled            bool
	PrimaryTLSCAFile             string // PEM bundle for validating the primary cert
	PrimaryTLSInsecureSkipVerify bool   // dev / self-signed only; never true in prod
	// Shadow queue and timeout configuration
	ShadowQueueSize            int
	ShadowReadTimeout          time.Duration
	ShadowDrainTimeout         time.Duration // Timeout for draining queue on connection close
	ShadowResponseDrainTimeout time.Duration // Timeout for draining response after each query (should be short)
	// Shadow TLS configuration (proxy as client)
	ShadowTLSEnabled  bool // Enable TLS for shadow connections
	ShadowTLSInsecure bool // Skip certificate verification (for self-signed certs)
	// Query logging configuration (writes to GCS for BigQuery analysis)
	QueryLogGCSBucket     string        // GCS bucket for query logs (empty = disabled)
	QueryLogGCSPrefix     string        // Path prefix within bucket
	QueryLogFlushInterval time.Duration // How often to flush logs to GCS
	QueryLogBatchSize     int           // Max entries before forced flush
	QueryLogBufferSize    int           // In-memory buffer size
	// Shadow query filtering (selective mirroring)
	ShadowFilterMode          string   // "include" or "exclude" (empty = disabled, shadow everything)
	ShadowFilterSQLOperations []string // SQL operation types to filter: SELECT, INSERT_OVERWRITE, SUBMIT_TASK, etc.
	ShadowFilterPatterns      []string // Regex patterns matched against full query text
	ShadowSampleRate          float64  // 0.0 to 1.0 — fraction of queries to shadow (1.0 = all)
	// Debug logging
	DebugLog bool // Enable verbose per-connection trace logging (DEBUG_LOG=true)
}

// debugf logs a message only when debug logging is enabled.
// Use this for per-connection trace logs that are too verbose for production.
func debugf(config *Config, format string, args ...any) {
	if config.DebugLog {
		log.Printf(format, args...)
	}
}

// isPostgresProtocol reports whether the configured protocol value selects the
// pgwire path. Accepts "postgres", "postgresql", and "pg" (case-insensitive).
func isPostgresProtocol(protocol string) bool {
	switch strings.ToLower(protocol) {
	case "postgres", "postgresql", "pg":
		return true
	}
	return false
}

func loadConfig() *Config {
	tlsEnabled := getEnv("TLS_ENABLED", "false") == "true"
	shadowTLSEnabled := getEnv("SHADOW_TLS_ENABLED", "false") == "true"
	shadowTLSInsecure := getEnv("SHADOW_TLS_INSECURE", "true") == "true" // Default true for dev

	// Protocol-aware port defaults: postgres listens on 5432, mysql on 3306.
	protocol := strings.ToLower(getEnv("PROTOCOL", "mysql"))
	defaultListen := ":3306"
	defaultBackendPort := "9030"
	if isPostgresProtocol(protocol) {
		defaultListen = ":5432"
		defaultBackendPort = "5432"
	}

	return &Config{
		Protocol:        protocol,
		ListenAddr:      getEnv("LISTEN_ADDR", defaultListen),
		PrimaryHost:     getEnv("PRIMARY_HOST", ""),
		PrimaryPort:     getEnv("PRIMARY_PORT", defaultBackendPort),
		PrimaryUser:     getEnv("PRIMARY_USER", "root"),
		PrimaryPassword: getEnv("PRIMARY_PASSWORD", ""),
		ShadowHost:      getEnv("SHADOW_HOST", ""),
		ShadowPort:      getEnv("SHADOW_PORT", defaultBackendPort),
		ShadowUser:      getEnv("SHADOW_USER", "root"),
		ShadowPassword:  getEnv("SHADOW_PASSWORD", ""),
		MetricsPort:     getEnv("METRICS_PORT", ":9090"),
		TLSEnabled:                   tlsEnabled,
		TLSCertFile:                  getEnv("TLS_CERT_FILE", "/certs/tls.crt"),
		TLSKeyFile:                   getEnv("TLS_KEY_FILE", "/certs/tls.key"),
		PrimaryTLSEnabled:            getEnv("PRIMARY_TLS_ENABLED", "false") == "true",
		PrimaryTLSCAFile:             getEnv("PRIMARY_TLS_CA_FILE", ""),
		PrimaryTLSInsecureSkipVerify: getEnv("PRIMARY_TLS_INSECURE_SKIP_VERIFY", "false") == "true",
		// Shadow queue and timeout configuration (with sensible defaults)
		ShadowQueueSize:            getEnvInt("SHADOW_QUEUE_SIZE", 10000),
		ShadowReadTimeout:          time.Duration(getEnvInt("SHADOW_READ_TIMEOUT_SECONDS", 30)) * time.Second,
		ShadowDrainTimeout:         time.Duration(getEnvInt("SHADOW_DRAIN_TIMEOUT_MS", 60000)) * time.Millisecond,        // 60s for connection close drain
		ShadowResponseDrainTimeout: time.Duration(getEnvInt("SHADOW_RESPONSE_DRAIN_TIMEOUT_MS", 100)) * time.Millisecond, // 100ms for per-query response drain
		// Shadow TLS configuration (proxy as client to shadow)
		ShadowTLSEnabled:  shadowTLSEnabled,
		ShadowTLSInsecure: shadowTLSInsecure,
		// Query logging configuration (writes to GCS for BigQuery analysis)
		// Query logging uses Hive-style partitioning for efficient BigQuery queries
		QueryLogGCSBucket:     getEnv("QUERY_LOG_GCS_BUCKET", ""), // Empty = disabled
		QueryLogGCSPrefix:     getEnv("QUERY_LOG_GCS_PREFIX", "query-logs"),
		QueryLogFlushInterval: time.Duration(getEnvInt("QUERY_LOG_FLUSH_INTERVAL_SECONDS", 120)) * time.Second, // 2 min default
		QueryLogBatchSize:     getEnvInt("QUERY_LOG_BATCH_SIZE", 1000),                                         // Larger batches = fewer files
		QueryLogBufferSize:    getEnvInt("QUERY_LOG_BUFFER_SIZE", 10000),
		// Shadow query filtering (selective mirroring)
		ShadowFilterMode:          getEnv("SHADOW_FILTER_MODE", ""),
		ShadowFilterSQLOperations: getEnvList("SHADOW_FILTER_SQL_OPERATIONS", nil),
		ShadowFilterPatterns:      getEnvList("SHADOW_FILTER_PATTERNS", nil),
		ShadowSampleRate:          getEnvFloat("SHADOW_SAMPLE_RATE", 1.0),
		// Debug logging (off by default; enable with DEBUG_LOG=true for per-connection traces)
		DebugLog: getEnv("DEBUG_LOG", "false") == "true",
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
		log.Printf("Warning: invalid integer value for %s: %s, using default %d", key, value, defaultValue)
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return floatVal
		}
		log.Printf("Warning: invalid float value for %s: %s, using default %f", key, value, defaultValue)
	}
	return defaultValue
}

// getEnvList reads a comma-separated environment variable into a string slice.
// Returns defaultValue if the variable is not set or empty.
func getEnvList(key string, defaultValue []string) []string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	parts := strings.Split(value, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return defaultValue
	}
	return result
}
