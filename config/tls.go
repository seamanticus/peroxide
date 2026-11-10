package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// TLS builds the server's TLS configuration from serve's -tls-cert, -tls-key
// and -tls-client-ca. Without -tls-cert and -tls-key it uses serve's own
// certificate (ServeCertificate); created says it was made just now. A set
// that can't make a configuration is an error: there is no fallback to plain
// text, which only serve's -plaintext asks for.
func TLS(certPath string, keyPath string, clientCAPath string) (cfg *tls.Config, created bool, err error) {
	var cert tls.Certificate
	switch {
	case certPath == "" && keyPath == "":
		if cert, created, err = ServeCertificate(); err != nil {
			return nil, false, err
		}
	case keyPath == "":
		return nil, false, fmt.Errorf("-tls-cert needs -tls-key, the certificate's private key")
	case certPath == "":
		return nil, false, fmt.Errorf("-tls-key needs -tls-cert, the certificate it is the key of")
	default:
		if cert, err = tls.LoadX509KeyPair(certPath, keyPath); err != nil {
			return nil, false, fmt.Errorf("cannot load -tls-cert %v and -tls-key %v: %w", certPath, keyPath, err)
		}
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Go's default today, stated so that it stays.
		MinVersion: tls.VersionTLS12,
	}

	if clientCAPath != "" {
		data, err := os.ReadFile(clientCAPath)
		if err != nil {
			return nil, false, fmt.Errorf("cannot read -tls-client-ca: %w", err)
		}
		pool, err := certPool(data)
		if err != nil {
			return nil, false, fmt.Errorf("-tls-client-ca %v: %w", clientCAPath, err)
		}
		tlsConfig.ClientCAs = pool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsConfig, created, nil
}

// certPool reads every PEM certificate of data. AppendCertsFromPEM skips what
// it can't parse and keeps going, so an empty or half-read file would refuse
// every client, or some, with nothing said: each block must be a certificate.
func certPool(data []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	n := 0
	for rest := data; ; n++ {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("holds a %v block, not a CA certificate", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("cannot read certificate %v: %w", n+1, err)
		}
		pool.AddCert(cert)
	}
	if n == 0 {
		return nil, fmt.Errorf("holds no PEM certificate")
	}
	return pool, nil
}
