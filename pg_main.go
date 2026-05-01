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

func runPostgresProxy(config *Config) {
	log.Printf("Configuration:")
	log.Printf("  Protocol: postgres")
	log.Printf("  Listen Addr: %s", config.ListenAddr)
	log.Printf("  Primary: %s:%s", config.PrimaryHost, config.PrimaryPort)
	log.Printf("  Query Log GCS Bucket: %s", config.QueryLogGCSBucket)
	if config.QueryLogGCSBucket != "" {
		log.Printf("  Query Log GCS Prefix: %s", config.QueryLogGCSPrefix)
		log.Printf("  Query Log Flush Interval: %v", config.QueryLogFlushInterval)
		log.Printf("  Query Log Batch Size: %d", config.QueryLogBatchSize)
		log.Printf("  Query Log Buffer Size: %d", config.QueryLogBufferSize)
	}

	primaryAddr := fmt.Sprintf("%s:%s", config.PrimaryHost, config.PrimaryPort)
	var (
		healthMu       sync.RWMutex
		primaryHealthy bool
	)

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		checkHealth := func() {
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

		checkHealth()
		for range ticker.C {
			checkHealth()
		}
	}()

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		})
		mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
			healthMu.RLock()
			isPrimaryUp := primaryHealthy
			healthMu.RUnlock()
			if !isPrimaryUp {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, `{"primary": false}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"primary": true}`)
		})
		mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			healthMu.RLock()
			isPrimaryUp := primaryHealthy
			healthMu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"primary": {"address": "%s", "healthy": %v}}`, primaryAddr, isPrimaryUp)
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

	var queryLogger *QueryLogger
	if config.QueryLogGCSBucket != "" {
		var err error
		queryLogger, err = NewQueryLogger(QueryLoggerConfig{
			GCSBucket:     config.QueryLogGCSBucket,
			GCSPrefix:     config.QueryLogGCSPrefix,
			FlushInterval: config.QueryLogFlushInterval,
			BatchSize:     config.QueryLogBatchSize,
			BufferSize:    config.QueryLogBufferSize,
		})
		if err != nil {
			log.Printf("Warning: failed to initialize query logger, continuing without: %v", err)
		} else {
			queryLogger.Start()
			defer queryLogger.Close()
			log.Printf("Query logger initialized (bucket=%s, prefix=%s)",
				config.QueryLogGCSBucket, config.QueryLogGCSPrefix)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	proxy := NewPgProxy(config)
	if err := proxy.Start(ctx, queryLogger); err != nil {
		cancel()
		log.Fatal(err) //nolint:gocritic // cancel() called explicitly above
	}
}
