# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in this project, please report it responsibly.

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please email **security@trmlabs.com** with:

- A description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

We will acknowledge receipt within 48 hours and aim to provide a fix within 7 days for critical issues.

## Scope

This project handles database credentials and proxies SQL query traffic. Security-relevant areas include:

- **Credential handling**: Primary and shadow passwords are passed via environment variables and used for MySQL native password authentication
- **TLS termination**: The proxy terminates TLS for client connections and can optionally connect to the shadow cluster over TLS
- **Query logging**: When enabled, full SQL query text is written to GCS -- users should be aware of potential PII in logged queries
- **Network exposure**: The proxy listens on a TCP port and accepts MySQL protocol connections

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest release | Yes |
| Older releases | Best effort |
