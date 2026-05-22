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

// loadListenerTLSConfig returns nil when listener TLS is disabled.
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

// loadBackendTLSConfig returns nil when backend TLS is disabled.
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

// upgradeClientTLS replies 'S' to the client's SSLRequest then wraps in tls.Server.
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

// upgradeBackendTLS sends an SSLRequest and wraps in tls.Client when accepted.
// Fails closed if the backend replies 'N' — operator asked for TLS, server refused.
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
