// main.go — Entry point for the StarRocks shadow traffic proxy. Loads configuration,
// starts the health checker, metrics HTTP server, and the TCP proxy.
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

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	config := loadConfig()

	// Validate config
	if config.PrimaryHost == "" {
		log.Fatal("PRIMARY_HOST is required")
	}

	// Dispatch on protocol. Postgres MVP runs in transparent-forward mode (no shadow yet).
	if isPostgresProtocol(config.Protocol) {
		runPostgresProxy(config)
		return
	}

	if config.ShadowHost == "" {
		log.Fatal("SHADOW_HOST is required")
	}

	// Log configuration
	log.Printf("Configuration:")
	log.Printf("  Shadow Queue Size: %d", config.ShadowQueueSize)
	log.Printf("  Shadow Read Timeout: %v", config.ShadowReadTimeout)
	log.Printf("  Shadow Drain Timeout: %v", config.ShadowDrainTimeout)
	log.Printf("  Shadow Response Drain Timeout: %v", config.ShadowResponseDrainTimeout)
	log.Printf("  TLS Enabled (client): %v", config.TLSEnabled)
	log.Printf("  Shadow TLS Enabled: %v", config.ShadowTLSEnabled)
	if config.ShadowTLSEnabled {
		log.Printf("  Shadow TLS Insecure: %v", config.ShadowTLSInsecure)
	}
	log.Printf("  Query Log GCS Bucket: %s", config.QueryLogGCSBucket)
	if config.QueryLogGCSBucket != "" {
		log.Printf("  Query Log GCS Prefix: %s", config.QueryLogGCSPrefix)
		log.Printf("  Query Log Flush Interval: %v", config.QueryLogFlushInterval)
		log.Printf("  Query Log Batch Size: %d", config.QueryLogBatchSize)
		log.Printf("  Query Log Buffer Size: %d", config.QueryLogBufferSize)
	}

	// Health check state
	var (
		healthMu       sync.RWMutex
		primaryHealthy = false
		shadowHealthy  = false
	)

	primaryAddr := fmt.Sprintf("%s:%s", config.PrimaryHost, config.PrimaryPort)
	shadowAddr := fmt.Sprintf("%s:%s", config.ShadowHost, config.ShadowPort)

	// Background health checker
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		checkHealth := func() {
			// Check primary
			conn, err := net.DialTimeout("tcp", primaryAddr, 3*time.Second)
			healthMu.Lock()
			if err != nil {
				primaryHealthy = false
				primaryUp.Set(0)
			} else {
				primaryHealthy = true
				primaryUp.Set(1)
				conn.Close()
			}
			healthMu.Unlock()

			// Check shadow
			conn, err = net.DialTimeout("tcp", shadowAddr, 3*time.Second)
			healthMu.Lock()
			if err != nil {
				shadowHealthy = false
				shadowUp.Set(0)
			} else {
				shadowHealthy = true
				shadowUp.Set(1)
				conn.Close()
			}
			healthMu.Unlock()
		}

		// Initial check
		checkHealth()

		for range ticker.C {
			checkHealth()
		}
	}()

	// Start metrics server
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		// Basic liveness probe - proxy process is alive
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		})

		// Readiness probe - primary must be reachable
		mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
			healthMu.RLock()
			isPrimaryUp := primaryHealthy
			isShadowUp := shadowHealthy
			healthMu.RUnlock()

			if !isPrimaryUp {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, `{"primary": false, "shadow": %v}`, isShadowUp)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"primary": true, "shadow": %v}`, isShadowUp)
		})

		// Detailed status endpoint
		mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			healthMu.RLock()
			isPrimaryUp := primaryHealthy
			isShadowUp := shadowHealthy
			healthMu.RUnlock()

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"primary": {"address": "%s", "healthy": %v}, "shadow": {"address": "%s", "healthy": %v}}`,
				primaryAddr, isPrimaryUp, shadowAddr, isShadowUp)
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

	// Create query filter for selective shadow mirroring
	queryFilter, err := NewQueryFilter(config)
	if err != nil {
		log.Fatalf("Failed to create query filter: %v", err)
	}
	if queryFilter != nil {
		log.Printf("  Shadow Query Filter: %s", queryFilter)
	} else {
		log.Printf("  Shadow Query Filter: disabled (mirroring all queries)")
	}

	// Create and start proxy
	proxy, err := NewTCPProxy(config)
	if err != nil {
		log.Fatalf("Failed to create proxy: %v", err)
	}
	proxy.queryFilter = queryFilter

	// Initialize query logger if GCS bucket is configured
	var queryLogger *QueryLogger
	if config.QueryLogGCSBucket != "" {
		var loggerErr error
		queryLogger, loggerErr = NewQueryLogger(QueryLoggerConfig{
			GCSBucket:     config.QueryLogGCSBucket,
			GCSPrefix:     config.QueryLogGCSPrefix,
			FlushInterval: config.QueryLogFlushInterval,
			BatchSize:     config.QueryLogBatchSize,
			BufferSize:    config.QueryLogBufferSize,
		})
		if loggerErr != nil {
			log.Printf("Warning: failed to initialize query logger, continuing without: %v", loggerErr)
		} else {
			queryLogger.Start()
			defer queryLogger.Close()
			log.Printf("Query logger initialized (bucket=%s, prefix=%s)",
				config.QueryLogGCSBucket, config.QueryLogGCSPrefix)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
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
