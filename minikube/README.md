# Minikube Test Environment for Shadow Proxy

This directory contains everything needed to run a production-like test environment for the Shadow Proxy using Minikube.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│ Minikube Cluster                                                                │
│                                                                                 │
│ ┌─────────────────────────────────────────────────────────────────────────────┐ │
│ │ starrocks-primary namespace                                                 │ │
│ │                                                                             │ │
│ │  ┌──────────────────┐     ┌────────────────────────────────────────────┐   │ │
│ │  │ StarRocks        │     │ Shadow Proxy                               │   │ │
│ │  │ Operator         │     │                                            │   │ │
│ │  │ (Primary)        │     │  Client ──[TLS]──> :3306 ──[TCP]──┬──> FE  │   │ │
│ │  └──────────────────┘     │                                   │        │   │ │
│ │                           │                                   └──> ────┼───┼─┤
│ │  ┌──────────────────┐     └────────────────────────────────────────────┘   │ │
│ │  │ Primary FE       │                                                       │ │
│ │  │ Service (:9030)  │◄──────────────────────────────────────────────────────┤ │
│ │  └────────┬─────────┘                                                       │ │
│ │           │                                                                 │ │
│ │  ┌────────▼─────────┐     ┌──────────────────┐                             │ │
│ │  │ FE Pod           │     │ BE Pod           │                             │ │
│ │  └──────────────────┘     └──────────────────┘                             │ │
│ └─────────────────────────────────────────────────────────────────────────────┘ │
│                                                                                 │
│ ┌─────────────────────────────────────────────────────────────────────────────┐ │
│ │ starrocks-shadow namespace                                                  │ │
│ │                                                                             │ │
│ │  ┌──────────────────┐                                                       │ │
│ │  │ StarRocks        │                                                       │ │
│ │  │ Operator         │                                                       │ │
│ │  │ (Shadow)         │                                                       │ │
│ │  └──────────────────┘                                                       │ │
│ │                                                                             │ │
│ │  ┌──────────────────┐                                                       │ │
│ │  │ Shadow FE        │◄───────────────────────────────────────────(from proxy)│
│ │  │ Service (:9030)  │                                                       │ │
│ │  └────────┬─────────┘                                                       │ │
│ │           │                                                                 │ │
│ │  ┌────────▼─────────┐     ┌──────────────────┐                             │ │
│ │  │ FE Pod           │     │ BE Pod           │                             │ │
│ │  └──────────────────┘     └──────────────────┘                             │ │
│ └─────────────────────────────────────────────────────────────────────────────┘ │
│                                                                                 │
│ ┌─────────────────────────────────────────────────────────────────────────────┐ │
│ │ monitoring namespace                                                        │ │
│ │                                                                             │ │
│ │  ┌──────────────────┐     ┌──────────────────┐                             │ │
│ │  │ Prometheus       │────▶│ Grafana          │                             │ │
│ │  │ (:9090)          │     │ (:3000)          │                             │ │
│ │  └──────────────────┘     └──────────────────┘                             │ │
│ └─────────────────────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

Install the following tools:

```bash
# Minikube
brew install minikube

# kubectl
brew install kubectl

# Helm
brew install helm

# Docker Desktop (must be running)
# Download from https://www.docker.com/products/docker-desktop
```

## Quick Start

### 1. Full Setup (First Time)

```bash
cd minikube

# Make scripts executable
chmod +x setup.sh test.sh

# Run full setup (takes ~10-15 minutes)
./setup.sh
```

This will:
- Start Minikube with 8 CPUs and 14GB memory
- Create three namespaces
- Install two separate StarRocks operators
- Deploy two StarRocks clusters (1 FE + 1 BE each)
- Generate TLS certificates
- Deploy the Shadow Proxy
- Set up Prometheus + Grafana monitoring

### 2. Run Tests

```bash
./test.sh
```

This validates:
- Direct connections to both StarRocks clusters
- Shadow Proxy connections (plain TCP and TLS)
- Query mirroring functionality
- Metrics collection

### 3. Manual Testing

Open multiple terminals and run:

**Terminal 1 - Port Forwards:**
```bash
# Primary StarRocks
kubectl port-forward svc/primary-starrocks-fe-service 9030:9030 -n starrocks-primary

# In another terminal
kubectl port-forward svc/shadow-starrocks-fe-service 9031:9030 -n starrocks-shadow

# Shadow Proxy
kubectl port-forward svc/shadow-proxy 3306:3306 -n starrocks-primary

# Grafana
kubectl port-forward svc/grafana 3000:3000 -n monitoring
```

**Terminal 2 - Test Queries:**
```bash
# Direct to Primary
mysql -h 127.0.0.1 -P 9030 -u root -proot123 -e "SELECT 'primary' as cluster;"

# Direct to Shadow
mysql -h 127.0.0.1 -P 9031 -u root -proot123 -e "SELECT 'shadow' as cluster;"

# Through Shadow Proxy (Plain TCP)
mysql -h 127.0.0.1 -P 3306 -u root -proot123 -e "SELECT 'via proxy' as source;"

# Through Shadow Proxy (TLS)
mysql -h 127.0.0.1 -P 3306 -u root -proot123 \
  --ssl-mode=REQUIRED \
  --ssl-ca=certs/ca.crt \
  -e "SELECT 'via proxy TLS' as source;"
```

### 4. Monitoring

Open Grafana at http://localhost:3000:
- Username: `admin`
- Password: `admin`
- Navigate to Dashboards → Shadow Proxy → Shadow Proxy Comparison

## Commands Reference

### Setup Commands

```bash
# Full setup
./setup.sh

# Just start Minikube
./setup.sh start

# Deploy everything (assumes Minikube running)
./setup.sh deploy

# Teardown everything
./setup.sh teardown
```

### Kubectl Commands

```bash
# Check all pods
kubectl get pods -A

# Check pod logs
kubectl logs -n starrocks-primary -l app=shadow-proxy -f

# Describe a failing pod
kubectl describe pod -n starrocks-primary <pod-name>

# Get FE service endpoints
kubectl get endpoints -n starrocks-primary

# Shell into a pod
kubectl exec -it -n starrocks-primary <pod-name> -- bash
```

### Minikube Commands

```bash
# Check status
minikube status

# SSH into Minikube VM
minikube ssh

# View dashboard
minikube dashboard

# Stop Minikube
minikube stop

# Delete everything
minikube delete
```

## Resource Usage

| Component | CPU Request | Memory Request |
|-----------|-------------|----------------|
| Primary FE | 500m | 2Gi |
| Primary BE | 1 | 4Gi |
| Shadow FE | 500m | 2Gi |
| Shadow BE | 1 | 4Gi |
| Shadow Proxy | 100m | 128Mi |
| Prometheus | 200m | 512Mi |
| Grafana | 100m | 256Mi |
| **Total** | **~4** | **~13Gi** |

Minikube is configured with 8 CPUs and 14GB memory to accommodate this.

## Troubleshooting

### Pods Not Starting

```bash
# Check events
kubectl get events -n starrocks-primary --sort-by='.lastTimestamp'

# Check pod status
kubectl describe pod -n starrocks-primary <pod-name>
```

### StarRocks FE Not Ready

FE pods need time to initialize. Wait 3-5 minutes after deployment.

```bash
# Check FE logs
kubectl logs -n starrocks-primary -l app.kubernetes.io/component=fe -f
```

### Shadow Proxy Connection Issues

```bash
# Check proxy logs
kubectl logs -n starrocks-primary -l app=shadow-proxy -f

# Check if backend services are reachable
kubectl exec -n starrocks-primary -l app=shadow-proxy -- \
  nc -zv primary-starrocks-fe-service 9030

kubectl exec -n starrocks-primary -l app=shadow-proxy -- \
  nc -zv shadow-starrocks-fe-service.starrocks-shadow.svc.cluster.local 9030
```

### TLS Issues

```bash
# Verify certificates exist
ls -la certs/

# Regenerate certificates
rm -f certs/*.crt certs/*.key
./setup.sh deploy
```

### Resource Constraints

If Minikube is slow or pods are being killed:

```bash
# Stop and restart with more resources
minikube stop
minikube delete
minikube start --cpus=10 --memory=16384 --disk-size=50g
./setup.sh deploy
```

## Files Structure

```
minikube/
├── setup.sh              # Main setup script
├── test.sh               # Test validation script
├── README.md             # This file
├── certs/                # Generated TLS certificates
│   ├── ca.crt
│   ├── ca.key
│   ├── server.crt
│   └── server.key
├── primary/              # Primary cluster manifests
│   ├── starrocks-cluster.yaml
│   └── shadow-proxy.yaml
├── shadow/               # Shadow cluster manifests
│   └── starrocks-cluster.yaml
└── monitoring/           # Monitoring stack
    ├── prometheus.yaml   # Prometheus deployment
    └── grafana.yaml      # Grafana deployment with dashboard
```

## Differences from Production

The Minikube setup uses minimal resources suitable for local development:

| Aspect | Minikube | Production |
|--------|----------|------------|
| FE Replicas | 1 | 3+ (HA) |
| BE Replicas | 1 | Multiple |
| FE Memory | 2Gi | Sized to workload |
| BE Memory | 4Gi | Sized to workload |
| Storage | EmptyDir | Persistent SSD |
| TLS Certs | Self-signed | cert-manager |
| Service Type | NodePort | LoadBalancer |

The core functionality and architecture remain the same, making this suitable for development and testing.
