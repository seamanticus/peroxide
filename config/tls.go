package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

func TLS(certPath string, keyPath string, clientCAPath string) (*tls.Config, error) {
	var tlsConfig *tls.Config

	if certPath != "" && keyPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("unable load key pair: %s", err)
		}

		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
	}

	if clientCAPath != "" {
		if tlsConfig == nil {
			return nil, fmt.Errorf("cannot check client certificate without a server certificate and key")
		}

		data, err := os.ReadFile(clientCAPath)
		if err != nil {
			return nil, fmt.Errorf("unable read CA file: %s", err)
		}

		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(data)

		tlsConfig.ClientCAs = pool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsConfig, nil
}
