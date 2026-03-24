#!/bin/bash
# One-click Minikube setup for StarRocks Shadow Proxy testing
# Prerequisites: Docker Desktop with 12GB+ memory, minikube, kubectl, helm

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "============================================="
echo "StarRocks Shadow Proxy - Minikube Setup"
echo "============================================="

# Check prerequisites
command -v minikube >/dev/null 2>&1 || { echo "minikube required but not installed"; exit 1; }
command -v kubectl >/dev/null 2>&1 || { echo "kubectl required but not installed"; exit 1; }
command -v helm >/dev/null 2>&1 || { echo "helm required but not installed"; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "docker required but not installed"; exit 1; }

# Configuration
MINIKUBE_MEMORY=11264  # 11GB (fits in Docker Desktop 12GB allocation)
MINIKUBE_CPUS=4
STARROCKS_OPERATOR_VERSION="v1.11.4"
STARROCKS_VERSION="4.0.3"

echo ""
echo "Step 1/8: Starting Minikube with ${MINIKUBE_MEMORY}MB memory..."
minikube delete 2>/dev/null || true
minikube config set memory $MINIKUBE_MEMORY
minikube config set cpus $MINIKUBE_CPUS
minikube start

echo ""
echo "Step 2/8: Pre-pulling Docker images..."
docker pull starrocks/operator:$STARROCKS_OPERATOR_VERSION &
docker pull starrocks/fe-ubuntu:$STARROCKS_VERSION &
docker pull starrocks/be-ubuntu:$STARROCKS_VERSION &
docker pull prom/prometheus:v2.47.0 &
docker pull grafana/grafana:10.2.0 &
wait

echo ""
echo "Step 3/8: Loading images into Minikube..."
minikube image load starrocks/operator:$STARROCKS_OPERATOR_VERSION &
minikube image load starrocks/fe-ubuntu:$STARROCKS_VERSION &
minikube image load starrocks/be-ubuntu:$STARROCKS_VERSION &
minikube image load prom/prometheus:v2.47.0 &
minikube image load grafana/grafana:10.2.0 &
wait

echo ""
echo "Step 4/8: Creating namespaces..."
kubectl create namespace starrocks-primary
kubectl create namespace starrocks-shadow
kubectl create namespace monitoring

echo ""
echo "Step 5/8: Installing StarRocks operator..."
helm repo add starrocks https://starrocks.github.io/starrocks-kubernetes-operator 2>/dev/null || true
helm repo update
helm install starrocks-operator starrocks/operator -n starrocks-primary \
  --set image.tag=$STARROCKS_OPERATOR_VERSION \
  --set image.pullPolicy=IfNotPresent \
  --wait --timeout 120s

# Ensure imagePullPolicy is set
kubectl patch deployment kube-starrocks-operator -n starrocks-primary \
  --type='json' -p='[{"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "IfNotPresent"}]' 2>/dev/null || true

echo ""
echo "Step 6/8: Deploying StarRocks clusters..."
kubectl apply -f primary/starrocks-cluster.yaml
kubectl apply -f shadow/starrocks-cluster.yaml

echo "Waiting for FEs to be ready..."
kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=fe -n starrocks-primary --timeout=300s
kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=fe -n starrocks-shadow --timeout=300s

echo "Waiting for BEs to be ready..."
kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=be -n starrocks-primary --timeout=300s
kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=be -n starrocks-shadow --timeout=300s

echo ""
echo "Step 7/8: Deploying Shadow Proxy..."

# Generate TLS certificates
mkdir -p certs
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout certs/tls.key -out certs/tls.crt \
  -subj "/CN=shadow-proxy" 2>/dev/null
cp certs/tls.crt certs/ca.crt

# Create TLS secret
kubectl create secret tls shadow-proxy-tls -n starrocks-primary \
  --cert=certs/tls.crt --key=certs/tls.key

# Build and load shadow proxy image
cd "$SCRIPT_DIR/.."
GOOS=linux GOARCH=arm64 go build -o starrocks-shadow-proxy . 2>/dev/null || \
GOOS=linux GOARCH=amd64 go build -o starrocks-shadow-proxy .
docker build -t shadow-proxy:latest . -q
minikube image load shadow-proxy:latest
cd "$SCRIPT_DIR"

# Deploy shadow proxy
kubectl apply -f primary/shadow-proxy.yaml

echo "Waiting for shadow proxy to be ready..."
kubectl wait --for=condition=ready pod -l app=shadow-proxy -n starrocks-primary --timeout=120s

echo ""
echo "Step 8/8: Deploying monitoring stack..."
kubectl apply -f monitoring/

echo "Waiting for monitoring to be ready..."
kubectl wait --for=condition=ready pod -l app=prometheus -n monitoring --timeout=120s
kubectl wait --for=condition=ready pod -l app=grafana -n monitoring --timeout=120s

# Wait for Grafana to be fully initialized
sleep 10

# Configure Grafana datasource
GRAFANA_POD=$(kubectl get pod -n monitoring -l app=grafana -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n monitoring $GRAFANA_POD -- curl -s -X POST "http://admin:admin@localhost:3000/api/datasources" \
  -H "Content-Type: application/json" \
  -d '{"name":"Prometheus","type":"prometheus","url":"http://prometheus.monitoring.svc.cluster.local:9090","access":"proxy","isDefault":true}' >/dev/null 2>&1 || true

echo ""
echo "============================================="
echo "SETUP COMPLETE!"
echo "============================================="
echo ""
echo "Starting port forwards..."

# Kill any existing port forwards
pkill -f "kubectl port-forward" 2>/dev/null || true
sleep 2

# Start port forwards in background
kubectl port-forward svc/shadow-proxy 3306:3306 -n starrocks-primary &
kubectl port-forward svc/grafana 3000:3000 -n monitoring &
kubectl port-forward svc/prometheus 9091:9090 -n monitoring &

sleep 3

echo ""
echo "============================================="
echo "ACCESS INFORMATION"
echo "============================================="
echo ""
echo "Grafana Dashboard:"
echo "  URL: http://localhost:3000"
echo "  Login: admin / admin"
echo "  Dashboard: StarRocks Shadow Proxy - Latency Comparison"
echo ""
echo "Shadow Proxy (TLS):"
echo "  mysql -h 127.0.0.1 -P 3306 -u root --ssl-mode=REQUIRED --ssl-ca=$SCRIPT_DIR/certs/ca.crt"
echo ""
echo "Prometheus:"
echo "  URL: http://localhost:9091"
echo ""
echo "Pods:"
kubectl get pods -A | grep -v kube-system
echo ""
echo "============================================="
