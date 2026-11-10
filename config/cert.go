package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// serve's own certificate, used when no -tls-cert is given. The certificate
// lives alone in its directory, so that a client can add the directory to
// its trust (SSL_CERT_DIR for Go programs, which read every file there); the
// key lives beside it in the configuration directory, owner only.
const (
	certDir     = "tls"
	certFile    = "cert.pem"
	certKeyFile = "tls-key.pem"
	// certLifetime is the certificate's validity: it is never renewed, as a
	// new one would have to be trusted again by every client. Its key can
	// only sign TLS handshakes for peroxide's names (it is no CA).
	certLifetime = 20 * 365 * 24 * time.Hour
)

// CertPath returns the path of serve's own certificate, creating nothing:
// `peroxide status` asks for it too.
func CertPath() (string, error) {
	configHome, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configHome, "peroxide", certDir, certFile), nil
}

func certKeyPath() (string, error) {
	return Path(certKeyFile)
}

// ServeCertificate returns serve's own certificate and key, creating them
// when there are none; created says it did. A key without a certificate is
// replaced: no client can trust a certificate never written. A certificate
// without its key, an expired one, or one its key doesn't match, is an
// error: clients may trust it, so it is never replaced unasked.
func ServeCertificate() (cert tls.Certificate, created bool, err error) {
	certPath, err := CertPath()
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("cannot find the TLS certificate: %w", err)
	}
	keyPath, err := certKeyPath()
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("cannot find the TLS key: %w", err)
	}

	certPEM, err := os.ReadFile(certPath)
	if errors.Is(err, os.ErrNotExist) {
		cert, err := createCertificate(certPath, keyPath)
		return cert, err == nil, err
	} else if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("cannot read the TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, false, fmt.Errorf("%v has no key (%v is missing): remove the certificate to have a new one made, and trust that one in your clients", certPath, keyPath)
	} else if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("cannot read the TLS key: %w", err)
	}

	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("%v and %v: %w", certPath, keyPath, err)
	}
	if now := time.Now(); now.After(cert.Leaf.NotAfter) {
		return tls.Certificate{}, false, fmt.Errorf("%v expired on %v: remove it and %v to have a new one made, and trust that one in your clients", certPath, cert.Leaf.NotAfter.Format(time.DateOnly), keyPath)
	}
	return cert, false, nil
}

// createCertificate makes a self-signed certificate for the loopback names
// and writes it to certPath and its key to keyPath, the key first: a crash
// between them leaves a key alone, which the next start replaces.
func createCertificate(certPath, keyPath string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cannot generate the TLS key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cannot generate a serial number: %w", err)
	}
	// An hour back, for a client whose clock is a little behind.
	notBefore := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "peroxide"},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(certLifetime),
		// No CA: trusted as its own anchor, it can't vouch for any other
		// certificate, unlike proton-bridge's (internal/certs/tls.go).
		BasicConstraintsValid: true,
		IsCA:                  false,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cannot make the TLS certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cannot encode the TLS key: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("cannot write the TLS key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("cannot make the TLS certificate's directory: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// Readable by its owner only, as everything in the configuration
	// directory: a client trusting it runs as the same user.
	if err := writeFileAtomic(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("cannot write the TLS certificate: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// writeFileAtomic writes data to p through a temporary file in its directory
// renamed over it, so that p is never seen half written, then syncs the
// directory, so that the new name outlives a crash: the certificate's must
// never survive without the key's, written just before.
func writeFileAtomic(p string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after the rename, there is none left to remove
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Fingerprint is der's SHA-256 in the form `openssl x509 -fingerprint
// -sha256` prints, for comparing a certificate by eye.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	hex := make([]string, len(sum))
	for i, b := range sum {
		hex[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(hex, ":")
}

// CertFileFingerprint is the Fingerprint of the first certificate in the PEM
// file p; an error wrapping os.ErrNotExist when there is no file.
func CertFileFingerprint(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	for rest := data; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return "", fmt.Errorf("%v holds no PEM certificate", p)
		}
		if block.Type == "CERTIFICATE" {
			return Fingerprint(block.Bytes), nil
		}
	}
}
