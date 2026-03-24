package main

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	// Set test environment variables
	os.Setenv("PRIMARY_HOST", "primary.example.com")
	os.Setenv("PRIMARY_PORT", "9030")
	os.Setenv("PRIMARY_USER", "testuser")
	os.Setenv("PRIMARY_PASSWORD", "testpass")
	os.Setenv("SHADOW_HOST", "shadow.example.com")
	os.Setenv("SHADOW_PORT", "9031")
	os.Setenv("SHADOW_USER", "shadowuser")
	os.Setenv("SHADOW_PASSWORD", "shadowpass")
	defer func() {
		os.Unsetenv("PRIMARY_HOST")
		os.Unsetenv("PRIMARY_PORT")
		os.Unsetenv("PRIMARY_USER")
		os.Unsetenv("PRIMARY_PASSWORD")
		os.Unsetenv("SHADOW_HOST")
		os.Unsetenv("SHADOW_PORT")
		os.Unsetenv("SHADOW_USER")
		os.Unsetenv("SHADOW_PASSWORD")
	}()

	config := loadConfig()

	if config.PrimaryHost != "primary.example.com" {
		t.Errorf("Expected PrimaryHost 'primary.example.com', got '%s'", config.PrimaryHost)
	}
	if config.PrimaryPort != "9030" {
		t.Errorf("Expected PrimaryPort '9030', got '%s'", config.PrimaryPort)
	}
	if config.ShadowHost != "shadow.example.com" {
		t.Errorf("Expected ShadowHost 'shadow.example.com', got '%s'", config.ShadowHost)
	}
}

func TestGetEnvDefault(t *testing.T) {
	// Test with unset variable
	os.Unsetenv("TEST_VAR_UNSET")
	result := getEnv("TEST_VAR_UNSET", "default_value")
	if result != "default_value" {
		t.Errorf("Expected 'default_value', got '%s'", result)
	}

	// Test with set variable
	os.Setenv("TEST_VAR_SET", "actual_value")
	defer os.Unsetenv("TEST_VAR_SET")
	result = getEnv("TEST_VAR_SET", "default_value")
	if result != "actual_value" {
		t.Errorf("Expected 'actual_value', got '%s'", result)
	}
}

// TestQueryLogConfigFromEnv tests that query log config is loaded from environment
func TestQueryLogConfigFromEnv(t *testing.T) {
	// Set environment variables
	os.Setenv("PRIMARY_HOST", "localhost")
	os.Setenv("SHADOW_HOST", "localhost")
	os.Setenv("QUERY_LOG_GCS_BUCKET", "test-bucket")
	os.Setenv("QUERY_LOG_GCS_PREFIX", "test-prefix/logs")
	os.Setenv("QUERY_LOG_FLUSH_INTERVAL_SECONDS", "60")
	os.Setenv("QUERY_LOG_BATCH_SIZE", "500")
	os.Setenv("QUERY_LOG_BUFFER_SIZE", "5000")
	defer func() {
		os.Unsetenv("PRIMARY_HOST")
		os.Unsetenv("SHADOW_HOST")
		os.Unsetenv("QUERY_LOG_GCS_BUCKET")
		os.Unsetenv("QUERY_LOG_GCS_PREFIX")
		os.Unsetenv("QUERY_LOG_FLUSH_INTERVAL_SECONDS")
		os.Unsetenv("QUERY_LOG_BATCH_SIZE")
		os.Unsetenv("QUERY_LOG_BUFFER_SIZE")
	}()

	config := loadConfig()

	if config.QueryLogGCSBucket != "test-bucket" {
		t.Errorf("Expected QueryLogGCSBucket 'test-bucket', got '%s'", config.QueryLogGCSBucket)
	}
	if config.QueryLogGCSPrefix != "test-prefix/logs" {
		t.Errorf("Expected QueryLogGCSPrefix 'test-prefix/logs', got '%s'", config.QueryLogGCSPrefix)
	}
	if config.QueryLogFlushInterval != 60*time.Second {
		t.Errorf("Expected QueryLogFlushInterval 60s, got %v", config.QueryLogFlushInterval)
	}
	if config.QueryLogBatchSize != 500 {
		t.Errorf("Expected QueryLogBatchSize 500, got %d", config.QueryLogBatchSize)
	}
	if config.QueryLogBufferSize != 5000 {
		t.Errorf("Expected QueryLogBufferSize 5000, got %d", config.QueryLogBufferSize)
	}
}

// TestQueryLogConfigDefaults tests default values when env vars are not set
func TestQueryLogConfigDefaults(t *testing.T) {
	// Ensure query log env vars are not set
	os.Unsetenv("QUERY_LOG_GCS_BUCKET")
	os.Unsetenv("QUERY_LOG_GCS_PREFIX")
	os.Unsetenv("QUERY_LOG_FLUSH_INTERVAL_SECONDS")
	os.Unsetenv("QUERY_LOG_BATCH_SIZE")
	os.Unsetenv("QUERY_LOG_BUFFER_SIZE")

	// Set required vars
	os.Setenv("PRIMARY_HOST", "localhost")
	os.Setenv("SHADOW_HOST", "localhost")
	defer func() {
		os.Unsetenv("PRIMARY_HOST")
		os.Unsetenv("SHADOW_HOST")
	}()

	config := loadConfig()

	// Verify defaults
	if config.QueryLogGCSBucket != "" {
		t.Errorf("Expected empty QueryLogGCSBucket by default, got '%s'", config.QueryLogGCSBucket)
	}
	if config.QueryLogGCSPrefix != "query-logs" {
		t.Errorf("Expected QueryLogGCSPrefix 'query-logs', got '%s'", config.QueryLogGCSPrefix)
	}
	if config.QueryLogFlushInterval != 120*time.Second {
		t.Errorf("Expected QueryLogFlushInterval 120s, got %v", config.QueryLogFlushInterval)
	}
	if config.QueryLogBatchSize != 1000 {
		t.Errorf("Expected QueryLogBatchSize 1000, got %d", config.QueryLogBatchSize)
	}
	if config.QueryLogBufferSize != 10000 {
		t.Errorf("Expected QueryLogBufferSize 10000, got %d", config.QueryLogBufferSize)
	}
}

// TestDebugLogConfig tests the DEBUG_LOG configuration flag
func TestDebugLogConfig(t *testing.T) {
	os.Setenv("PRIMARY_HOST", "localhost")
	os.Setenv("SHADOW_HOST", "localhost")
	defer func() {
		os.Unsetenv("PRIMARY_HOST")
		os.Unsetenv("SHADOW_HOST")
		os.Unsetenv("DEBUG_LOG")
	}()

	// Default: debug log disabled
	os.Unsetenv("DEBUG_LOG")
	config := loadConfig()
	if config.DebugLog {
		t.Error("Expected DebugLog to be false by default")
	}

	// Explicitly enabled
	os.Setenv("DEBUG_LOG", "true")
	config = loadConfig()
	if !config.DebugLog {
		t.Error("Expected DebugLog to be true when DEBUG_LOG=true")
	}

	// Explicitly disabled
	os.Setenv("DEBUG_LOG", "false")
	config = loadConfig()
	if config.DebugLog {
		t.Error("Expected DebugLog to be false when DEBUG_LOG=false")
	}
}
