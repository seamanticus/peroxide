package tlscert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The first call makes the certificate, later ones load the same one. It is
// a server certificate for the loopback names only, no CA, its own anchor;
// the key is owner only, and the certificate alone in its directory.
// evidence: witnesses
func TestServeCertificateCreatedOnce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cert, created, err := ServeCertificate()
	if err != nil || !created {
		t.Fatalf("ServeCertificate() first = %v, %v; want created", created, err)
	}
	again, created, err := ServeCertificate()
	if err != nil || created {
		t.Fatalf("ServeCertificate() second = %v, %v; want loaded", created, err)
	}
	if !bytes.Equal(cert.Certificate[0], again.Certificate[0]) {
		t.Error("the second call returned another certificate")
	}

	leaf := cert.Leaf
	if leaf.IsCA || !leaf.BasicConstraintsValid {
		t.Error("the certificate may act as a CA")
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("key usage = %v, want digital signature only", leaf.KeyUsage)
	}
	if !slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Errorf("extended key usage = %v, want server auth only", leaf.ExtKeyUsage)
	}
	if d := leaf.NotAfter.Sub(leaf.NotBefore); d != certLifetime {
		t.Errorf("validity = %v, want %v", d, certLifetime)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	for _, name := range []string{"localhost", "127.0.0.1", "::1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Errorf("trusted alone, it doesn't verify for %v: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "example.org"}); err == nil {
		t.Error("it verifies for example.org")
	}

	certPath, err := CertPath()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(certPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != certFile {
		t.Errorf("certificate directory holds %v, want only %v", entries, certFile)
	}
	keyPath, err := certKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{certPath, keyPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%v mode = %v, want 0600", filepath.Base(p), got)
		}
	}
}

// A certificate clients may trust is never replaced unasked: one without its
// key, with another key, or expired, is an error naming the fix. A key with
// no certificate (a crash between the writes) is replaced.
// evidence: witnesses
func TestServeCertificateBroken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		damage  func(t *testing.T, certPath, keyPath string)
		wantErr string // "" for a new certificate
	}{
		{"key alone", func(t *testing.T, certPath, _ string) { remove(t, certPath) }, ""},
		{"certificate alone", func(t *testing.T, _, keyPath string) { remove(t, keyPath) }, "has no key"},
		{"another key", func(t *testing.T, _, keyPath string) {
			writeKey(t, keyPath, newKey(t))
		}, "private key does not match public key"},
		{"expired", func(t *testing.T, certPath, keyPath string) {
			key := newKey(t)
			writeKey(t, keyPath, key)
			tmpl := &x509.Certificate{
				SerialNumber: big.NewInt(1),
				NotBefore:    time.Now().Add(-48 * time.Hour),
				NotAfter:     time.Now().Add(-24 * time.Hour),
			}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			write(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		}, "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			old, _, err := ServeCertificate()
			if err != nil {
				t.Fatal(err)
			}
			certPath, err := CertPath()
			if err != nil {
				t.Fatal(err)
			}
			keyPath, err := certKeyPath()
			if err != nil {
				t.Fatal(err)
			}
			tc.damage(t, certPath, keyPath)
			before, _ := os.ReadFile(certPath)

			cert, created, err := ServeCertificate()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ServeCertificate() = %v, want an error with %q", err, tc.wantErr)
				}
				if after, _ := os.ReadFile(certPath); !bytes.Equal(before, after) {
					t.Error("the certificate was changed")
				}
				return
			}
			if err != nil || !created {
				t.Fatalf("ServeCertificate() = %v, %v; want a new one", created, err)
			}
			if bytes.Equal(cert.Certificate[0], old.Certificate[0]) {
				t.Error("the old certificate came back")
			}
		})
	}
}

// evidence: witnesses
func TestFingerprint(t *testing.T) {
	// SHA-256 of the empty string.
	const want = "E3:B0:C4:42:98:FC:1C:14:9A:FB:F4:C8:99:6F:B9:24:27:AE:41:E4:64:9B:93:4C:A4:95:99:1B:78:52:B8:55"
	if got := Fingerprint(nil); got != want {
		t.Errorf("Fingerprint(nil) = %v, want %v", got, want)
	}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func writeKey(t *testing.T, p string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write(t, p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func write(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}
