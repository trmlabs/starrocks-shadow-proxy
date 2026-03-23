# Contributing to StarRocks Shadow Proxy

Thank you for your interest in contributing! This document provides guidelines for contributing to the project.

## Getting Started

### Prerequisites

- Go 1.24+ ([install](https://go.dev/doc/install))
- Docker and Docker Compose (for integration testing)
- `golangci-lint` (optional, for linting)

### Build and Test

```bash
# Build
make build

# Run unit tests
make test-unit

# Run all tests with race detection
make test

# Lint
make lint

# Format
make fmt
```

### Local Development

Start a full local environment with two StarRocks clusters, the proxy, Prometheus, and Grafana:

```bash
docker compose -f docker-compose.local.yaml up --build
```

See the [README](README.md) for more details on local testing with and without TLS.

## Making Changes

1. Fork the repository and create a branch from `main`
2. Make your changes
3. Add or update tests as appropriate
4. Run `make test` and `make lint` to verify
5. Open a pull request

### Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Run `make fmt` before committing
- Keep the single `package main` structure -- the codebase is intentionally flat

### Pull Requests

- Keep PRs focused on a single change
- Include a clear description of what changed and why
- Ensure all tests pass and there are no lint warnings
- Update documentation if behavior changes

## Reporting Issues

Open a [GitHub issue](https://github.com/trmlabs/starrocks-shadow-proxy/issues) with:

- A clear description of the problem
- Steps to reproduce
- Expected vs actual behavior
- Go version and OS

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
