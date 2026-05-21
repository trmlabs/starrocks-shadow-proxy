// pg_tls.go — TLS handshake helpers for the pgwire shadow proxy.
//
// Two flows are supported, each independently enabled via Config:
//
//   1. Client-side termination (Config.TLSEnabled):
//      Client → SSLRequest → proxy. Proxy replies 'S' (accept), wraps the
//      client connection in tls.Server() using the configured cert+key, and
//      then reads the real StartupMessage off the TLS-protected stream.
//
//   2. Backend-side initiation (Config.PrimaryTLSEnabled):
//      After the proxy has dialed the primary over plain TCP, the proxy
//      itself sends a pgwire SSLRequest, reads the server's 1-byte response,
//      and upgrades the backend connection via tls.Client() if the server
//      agrees ('S'). The proxy then proceeds with the StartupMessage flow on
//      the TLS-protected stream.
//
// These two flows are independent. A common production configuration is:
//   TLSEnabled=false   (pgbouncer → proxy stays plaintext within the VPC)
//   PrimaryTLSEnabled=true   (proxy → AlloyDB requires TLS by pg_hba)
//
// Local dev with `docker-compose.pg-tls.yaml` sets both to true so the entire
// chain is encrypted end-to-end, mirroring a future cert-fronted deploy.

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
)

// loadListenerTLSConfig returns a *tls.Config the proxy uses to terminate TLS
// from clients. Returns nil when TLS is not configured for the listener.
func loadListenerTLSConfig(config *Config) (*tls.Config, error) {
	if !config.TLSEnabled {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(config.TLSCertFile, config.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load listener cert/key (%s / %s): %w",
			config.TLSCertFile, config.TLSKeyFile, err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// loadBackendTLSConfig returns a *tls.Config the proxy uses when dialing the
// primary over TLS. Returns nil when backend TLS is disabled.
func loadBackendTLSConfig(config *Config) (*tls.Config, error) {
	if !config.PrimaryTLSEnabled {
		return nil, nil
	}
	tlsConf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: config.PrimaryTLSInsecureSkipVerify, //nolint:gosec // dev opt-in only; docs warn prod
		ServerName:         config.PrimaryHost,
	}
	if config.PrimaryTLSCAFile != "" {
		caPEM, err := os.ReadFile(config.PrimaryTLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read primary CA file %s: %w", config.PrimaryTLSCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("primary CA file %s contained no usable certs", config.PrimaryTLSCAFile)
		}
		tlsConf.RootCAs = pool
	}
	return tlsConf, nil
}

// upgradeClientTLS replies 'S' to a client's SSLRequest and wraps the
// connection in tls.Server. Caller must have already consumed the SSLRequest
// bytes from `client`.
func upgradeClientTLS(client net.Conn, listenerTLSConfig *tls.Config) (*tls.Conn, error) {
	if _, err := client.Write([]byte{'S'}); err != nil {
		return nil, fmt.Errorf("write SSLRequest 'S' ack to client: %w", err)
	}
	tlsConn := tls.Server(client, listenerTLSConfig)
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("client TLS handshake: %w", err)
	}
	return tlsConn, nil
}

// upgradeBackendTLS initiates a pgwire SSLRequest against `primary` and
// upgrades the connection to TLS if the server agrees. Returns the
// TLS-wrapped connection. The caller must not have sent any bytes yet.
//
// On wire:
//   - we send: SSLRequest (length=8, magic=80877103) → 8 bytes
//   - server replies: 1 byte, 'S' to accept or 'N' to reject
//
// If the server replies 'N' or anything else when PrimaryTLSEnabled is set,
// it is a hard error: the operator asked for TLS to the backend, but the
// backend refuses. Failing closed is better than silently degrading.
func upgradeBackendTLS(primary net.Conn, backendTLSConfig *tls.Config) (*tls.Conn, error) {
	sslReq := make([]byte, 8)
	binary.BigEndian.PutUint32(sslReq[0:4], 8)
	binary.BigEndian.PutUint32(sslReq[4:8], pgMagicSSLRequest)
	if _, err := primary.Write(sslReq); err != nil {
		return nil, fmt.Errorf("send SSLRequest to primary: %w", err)
	}
	resp := make([]byte, 1)
	if _, err := io.ReadFull(primary, resp); err != nil {
		return nil, fmt.Errorf("read SSLRequest response from primary: %w", err)
	}
	if resp[0] != 'S' {
		return nil, fmt.Errorf("primary refused TLS upgrade (replied 0x%02x); PRIMARY_TLS_ENABLED is set but the backend will not accept TLS", resp[0])
	}
	tlsConn := tls.Client(primary, backendTLSConfig)
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("backend TLS handshake: %w", err)
	}
	return tlsConn, nil
}
