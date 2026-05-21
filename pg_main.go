// pg_main.go — Bootstrap path for the pgwire shadow proxy.
//
// Mirrors main()'s MySQL bootstrap: starts the metrics server, the health checker,
// the optional GCS query logger, and the PgProxy. Kept in a separate file so the
// existing MySQL bootstrap (main.go) remains unchanged and easy to diff.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// runPostgresProxy is the entry point when PROTOCOL=postgres. It owns process
// lifecycle (signal handling, listener startup, graceful shutdown).
//
// Differences vs. the MySQL path (main.go):
//   - No SHADOW_HOST validation: PR #1 only does primary forwarding.
//   - PgProxy is constructed instead of TCPProxy.
//   - Health checker only checks the primary if SHADOW_HOST is unset.
func runPostgresProxy(config *Config) {
	log.Printf("Configuration (postgres):")
	log.Printf("  Protocol:           %s", config.Protocol)
	log.Printf("  Listen Addr:        %s", config.ListenAddr)
	log.Printf("  Primary:            %s:%s", config.PrimaryHost, config.PrimaryPort)
	log.Printf("  Listener TLS:       %v", config.TLSEnabled)
	log.Printf("  Backend TLS:        %v (insecure_skip_verify=%v)", config.PrimaryTLSEnabled, config.PrimaryTLSInsecureSkipVerify)
	if config.ShadowHost != "" {
		log.Printf("  Shadow (configured but not yet wired in PR #1): %s:%s", config.ShadowHost, config.ShadowPort)
	}
	log.Printf("  Query Log GCS Bucket: %s", config.QueryLogGCSBucket)
	if config.QueryLogGCSBucket != "" {
		log.Printf("  Query Log GCS Prefix:   %s", config.QueryLogGCSPrefix)
		log.Printf("  Query Log Flush Interval: %v", config.QueryLogFlushInterval)
		log.Printf("  Query Log Batch Size:   %d", config.QueryLogBatchSize)
	}

	listenerTLSConfig, err := loadListenerTLSConfig(config)
	if err != nil {
		log.Fatalf("Listener TLS config: %v", err)
	}
	backendTLSConfig, err := loadBackendTLSConfig(config)
	if err != nil {
		log.Fatalf("Backend TLS config: %v", err)
	}

	primaryAddr := fmt.Sprintf("%s:%s", config.PrimaryHost, config.PrimaryPort)

	// Health-check goroutine: pings the primary every 10s.
	var (
		healthMu       sync.RWMutex
		primaryHealthy bool
	)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		check := func() {
			conn, err := net.DialTimeout("tcp", primaryAddr, 3*time.Second)
			healthMu.Lock()
			defer healthMu.Unlock()
			if err != nil {
				primaryHealthy = false
				primaryUp.Set(0)
				return
			}
			primaryHealthy = true
			primaryUp.Set(1)
			conn.Close()
		}
		check()
		for range ticker.C {
			check()
		}
	}()

	// Metrics + health HTTP server.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		})
		mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
			healthMu.RLock()
			ok := primaryHealthy
			healthMu.RUnlock()
			if !ok {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, `{"primary": false}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"primary": true}`)
		})
		mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			healthMu.RLock()
			ok := primaryHealthy
			healthMu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"primary": {"address": "%s", "healthy": %v}}`, primaryAddr, ok)
		})
		server := &http.Server{
			Addr:         config.MetricsPort,
			Handler:      mux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  60 * time.Second,
		}
		log.Printf("Metrics server listening on %s", config.MetricsPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Metrics server error: %v", err)
		}
	}()

	// Optional GCS query logger.
	var queryLogger *QueryLogger
	if config.QueryLogGCSBucket != "" {
		ql, err := NewQueryLogger(QueryLoggerConfig{
			GCSBucket:     config.QueryLogGCSBucket,
			GCSPrefix:     config.QueryLogGCSPrefix,
			FlushInterval: config.QueryLogFlushInterval,
			BatchSize:     config.QueryLogBatchSize,
			BufferSize:    config.QueryLogBufferSize,
		})
		if err != nil {
			log.Printf("Warning: failed to initialize query logger, continuing without: %v", err)
		} else {
			ql.Start()
			defer ql.Close()
			queryLogger = ql
			log.Printf("Query logger initialized (bucket=%s, prefix=%s)",
				config.QueryLogGCSBucket, config.QueryLogGCSPrefix)
		}
	}

	proxy := NewPgProxy(config, listenerTLSConfig, backendTLSConfig)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	if err := proxy.Start(ctx, queryLogger); err != nil {
		cancel()
		log.Fatal(err) //nolint:gocritic // cancel() called explicitly above
	}
}
