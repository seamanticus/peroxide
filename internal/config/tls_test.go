package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePEM writes blocks to a file in dir and returns its path.
func writePEM(t *testing.T, dir, name string, blocks ...*pem.Block) string {
	t.Helper()
	var data []byte
	for _, b := range blocks {
		data = append(data, pem.EncodeToMemory(b)...)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Without a certificate of the user's, serve's own is used; a TLS option that
// can't be honoured is an error naming the option, never a plain-text server.
// evidence: witnesses
func TestTLSOptions(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certBlock := &pem.Block{Type: "CERTIFICATE", Bytes: der}
	keyBlock := &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}
	cert := writePEM(t, dir, "cert.pem", certBlock)
	keyFile := writePEM(t, dir, "key.pem", keyBlock)
	empty := writePEM(t, dir, "empty.pem")
	notCert := writePEM(t, dir, "notcert.pem", certBlock, keyBlock)
	missing := filepath.Join(dir, "missing.pem")

	for _, tc := range []struct {
		name          string
		cert, key, ca string
		wantErr       string // "" for success
		// On success: serve's own certificate, else the one given; client
		// certificates checked.
		wantOwn, wantCheck bool
	}{
		{"none", "", "", "", "", true, false},
		{"CA alone", "", "", cert, "", true, true},
		{"cert and key", cert, keyFile, "", "", false, false},
		{"cert, key and CA", cert, keyFile, cert, "", false, true},

		{"cert without key", cert, "", "", "-tls-cert needs -tls-key", false, false},
		{"key without cert", "", keyFile, "", "-tls-key needs -tls-cert", false, false},
		{"CA with cert only", cert, "", cert, "-tls-cert needs -tls-key", false, false},
		{"cert unreadable", missing, keyFile, "", "cannot load -tls-cert", false, false},
		{"key not the cert's", keyFile, cert, "", "cannot load -tls-cert", false, false},
		{"CA unreadable", cert, keyFile, missing, "cannot read -tls-client-ca", false, false},
		{"CA empty", cert, keyFile, empty, "holds no PEM certificate", false, false},
		{"CA holds a key", cert, keyFile, notCert, "holds a PRIVATE KEY block", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			c, created, err := TLS(tc.cert, tc.key, tc.ca)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("TLS() = %v, %v; want an error with %q", c, err, tc.wantErr)
				}
				if c != nil {
					t.Error("TLS() returned a configuration with its error")
				}
				return
			}
			if err != nil {
				t.Fatalf("TLS() = %v", err)
			}
			if c == nil {
				t.Fatal("TLS() = nil configuration: plain text")
			}
			if created != tc.wantOwn {
				t.Errorf("created = %v, want %v", created, tc.wantOwn)
			}
			want := der
			if tc.wantOwn {
				own, _, err := ServeCertificate()
				if err != nil {
					t.Fatal(err)
				}
				want = own.Certificate[0]
			}
			if len(c.Certificates) != 1 || !bytes.Equal(c.Certificates[0].Certificate[0], want) {
				t.Errorf("served certificate isn't the expected one (own %v)", tc.wantOwn)
			}
			if c.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion = %x, want TLS 1.2", c.MinVersion)
			}
			if checks := c.ClientAuth == tls.RequireAndVerifyClientCert && c.ClientCAs != nil; checks != tc.wantCheck {
				t.Errorf("client certificates checked = %v, want %v", checks, tc.wantCheck)
			}
		})
	}
}
