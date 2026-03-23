#!/bin/bash
# Generate self-signed TLS certificates for local testing
# Run from the certs/ directory

set -e

CERT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$CERT_DIR"

echo "Generating self-signed certificates for TLS testing..."

# Generate CA key and certificate
openssl genrsa -out ca.key 4096
openssl req -new -x509 -days 365 -key ca.key -out ca.crt \
    -subj "/C=US/ST=California/L=San Francisco/O=Shadow Proxy/CN=Shadow-Proxy-CA"

# Generate server key
openssl genrsa -out server.key 2048

# Create config file for SAN (Subject Alternative Names)
cat > server.conf << EOF
[req]
default_bits = 2048
prompt = no
default_md = sha256
distinguished_name = dn
req_extensions = req_ext

[dn]
C = US
ST = California
L = San Francisco
O = Shadow Proxy
CN = starrocks

[req_ext]
subjectAltName = @alt_names

[alt_names]
DNS.1 = localhost
DNS.2 = primary-fe
DNS.3 = shadow-fe
DNS.4 = shadow-proxy
DNS.5 = stunnel
DNS.6 = *.local
IP.1 = 127.0.0.1
EOF

# Generate CSR
openssl req -new -key server.key -out server.csr -config server.conf

# Generate server certificate signed by CA
openssl x509 -req -days 365 -in server.csr \
    -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out server.crt \
    -extensions req_ext -extfile server.conf

# Create PKCS12 keystore for StarRocks FE (requires Java keystore format)
openssl pkcs12 -export -in server.crt -inkey server.key \
    -out keystore.p12 -name starrocks \
    -CAfile ca.crt -caname root \
    -password pass:changeit

# Create PEM files for the Go proxy
cp server.crt tls.crt
cp server.key tls.key
cp ca.crt ca.pem

# Set permissions
chmod 644 *.crt *.pem tls.crt
chmod 600 *.key tls.key keystore.p12

echo ""
echo "Certificates generated successfully!"
echo "  - CA:          ca.crt, ca.key"
echo "  - Server:      server.crt, server.key"
echo "  - Keystore:    keystore.p12 (password: changeit)"
echo "  - Proxy TLS:   tls.crt, tls.key"
