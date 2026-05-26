.PHONY: build build-linux test test-unit test-integration run clean docker-build docker-push

# Variables
BINARY_NAME=starrocks-shadow-proxy
IMAGE_NAME=ghcr.io/trmlabs/starrocks-shadow-proxy
VERSION?=latest

# Build for current platform
build:
	go build -o $(BINARY_NAME) .

# Build for Linux (required for Docker)
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -o $(BINARY_NAME) .

# Build for Linux ARM64
build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -a -installsuffix cgo -o $(BINARY_NAME) .

# Run unit tests
test-unit:
	@go test -v -race -short ./... 2>&1 | tee /dev/stderr | awk '/^--- PASS/{pass++} /^--- FAIL/{fail++} END{printf "\n========================================\n  SUMMARY: %d passed, %d failed\n========================================\n", pass+0, fail+0; if(fail>0) exit 1}'

# Run all tests including integration
test:
	@go test -v -race ./... 2>&1 | tee /dev/stderr | awk '/^--- PASS/{pass++} /^--- FAIL/{fail++} END{printf "\n========================================\n  SUMMARY: %d passed, %d failed\n========================================\n", pass+0, fail+0; if(fail>0) exit 1}'

# Run benchmarks
bench:
	go test -bench=. -benchmem ./...

# Run with coverage
test-coverage:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Run locally (requires PRIMARY_HOST, SHADOW_HOST env vars)
run:
	go run .

# Start local test environment with Docker Compose
test-local: build-linux
	docker-compose -f docker-compose.local.yaml up --build

# Stop local test environment
test-local-down:
	docker-compose -f docker-compose.local.yaml down -v

# Start local Postgres test environment (proxy + 2 vanilla Postgres backends)
test-pg-local:
	./test-pg-local.sh

# Start local Postgres environment without smoke-test, leaving it up for manual use
test-pg-local-up:
	./test-pg-local.sh --keep

# Stop local Postgres test environment
test-pg-local-down:
	docker-compose -f docker-compose.pg.yaml down -v

# Start local TLS-required Postgres test environment (mirrors AlloyDB posture).
# Used to develop the proxy's TLS-termination path locally without round-trips
# through GitHub Actions. Requires self-signed certs (run ./certs/generate-certs.sh
# once before this target).
test-pg-local-tls:
	./test-pg-local-tls.sh

# Same as test-pg-local-tls but leaves the stack running for manual iteration.
test-pg-local-tls-up:
	./test-pg-local-tls.sh --keep

# Run only the direct-backend phases; useful while TLS termination is being
# implemented and the proxy is expected to fail.
test-pg-local-tls-backends:
	./test-pg-local-tls.sh --skip-proxy --keep

# Stop the TLS Postgres test environment.
test-pg-local-tls-down:
	docker-compose -f docker-compose.pg-tls.yaml down -v

# Manual integration test against local environment
test-integration:
	@echo "Starting test environment..."
	$(MAKE) build-linux
	docker-compose -f docker-compose.test.yaml up -d --build
	@echo "Waiting for services to be ready..."
	sleep 10
	@echo "Running integration tests..."
	@echo "Testing connection to proxy..."
	mysql -h 127.0.0.1 -P 3306 -u root -pprimarypass -e "SELECT 1 as test"
	@echo "Checking metrics..."
	curl -s http://localhost:9090/metrics | grep shadow_proxy
	@echo "Tests passed!"
	docker-compose -f docker-compose.test.yaml down -v

# Filter integration test — tests selective query filtering against real StarRocks
# Runs 4 phases: baseline, operation filter, pattern filter, include-only
# Requires: shadow-proxy:local-filter Docker image (build with make docker-build)
test-filter:
	@echo "Running filter integration tests (real StarRocks)..."
	./test-filter-integration.sh

# Docker build (multi-stage: builds Go binary inside Docker)
docker-build:
	docker build -t $(IMAGE_NAME):$(VERSION) .

# Docker build for ARM64
docker-build-arm64:
	docker build --platform linux/arm64 -t $(IMAGE_NAME):$(VERSION) .

# Docker push
docker-push:
	docker push $(IMAGE_NAME):$(VERSION)

# Docker build and push
docker-release: docker-build docker-push

# Docker build and push for ARM64
docker-release-arm64: docker-build-arm64 docker-push

# Clean
clean:
	rm -f $(BINARY_NAME)
	rm -f coverage.out coverage.html

# Lint
lint:
	golangci-lint run

# Format
fmt:
	go fmt ./...

# Download dependencies
deps:
	go mod download
	go mod tidy

# Help
help:
	@echo "Available targets:"
	@echo "  build              - Build binary for current platform"
	@echo "  build-linux        - Build binary for Linux amd64 (for Docker)"
	@echo "  build-linux-arm64  - Build binary for Linux arm64 (for GKE)"
	@echo "  test-unit          - Run unit tests"
	@echo "  test               - Run all tests"
	@echo "  bench              - Run benchmarks"
	@echo "  test-coverage      - Run tests with coverage report"
	@echo "  test-local         - Start local test environment (StarRocks/MySQL)"
	@echo "  test-local-down    - Stop local test environment (StarRocks/MySQL)"
	@echo "  test-pg-local              - Run Postgres smoke test (compose up + assertions + tear down)"
	@echo "  test-pg-local-up           - Start Postgres test environment and leave running"
	@echo "  test-pg-local-down         - Stop Postgres test environment"
	@echo "  test-pg-local-tls          - Run TLS-required Postgres smoke test (mirrors AlloyDB posture)"
	@echo "  test-pg-local-tls-up       - Start TLS Postgres test environment and leave running"
	@echo "  test-pg-local-tls-backends - Direct-backend TLS phases only (proxy expected to fail)"
	@echo "  test-pg-local-tls-down     - Stop TLS Postgres test environment"
	@echo "  test-integration   - Run full integration test"
	@echo "  test-filter        - Run filter integration test (Docker)"
	@echo "  docker-build       - Build Linux binary + Docker image (amd64)"
	@echo "  docker-build-arm64 - Build Linux binary + Docker image (arm64)"
	@echo "  docker-release     - Build and push Docker image (amd64)"
	@echo "  docker-release-arm64 - Build and push Docker image (arm64)"
	@echo "  clean              - Clean build artifacts"
	@echo "  deps               - Download Go dependencies"
